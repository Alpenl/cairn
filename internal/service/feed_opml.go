package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	feedremote "webtag/internal/feed"
	"webtag/internal/httperr"
	"webtag/internal/model"
)

const maxOPMLSubscriptions = 500

const (
	maxOPMLOutlineNodes = 5000
	maxOPMLDepth        = 32
)

type opmlDocument struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr,omitempty"`
	Head    opmlHead `xml:"head"`
	Body    opmlBody `xml:"body"`
}

type opmlHead struct {
	Title       string `xml:"title,omitempty"`
	DateCreated string `xml:"dateCreated,omitempty"`
}

type opmlBody struct {
	Outlines []opmlOutline `xml:"outline"`
}

type opmlOutline struct {
	Text     string        `xml:"text,attr,omitempty"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string        `xml:"htmlUrl,attr,omitempty"`
	Outlines []opmlOutline `xml:"outline"`
}

func (s *FeedService) ExportOPML(ctx context.Context) ([]byte, error) {
	overview, err := s.store.ListOverview(ctx, "")
	if err != nil {
		return nil, err
	}
	folderOutlines := make(map[uuid.UUID]*opmlOutline, len(overview.Folders))
	rootFeeds := make([]opmlOutline, 0)
	for _, folder := range overview.Folders {
		outline := &opmlOutline{Text: folder.Name, Title: folder.Name}
		folderOutlines[folder.ID] = outline
	}
	for _, subscription := range overview.Subscriptions {
		feedOutline := opmlOutline{
			Text:    subscription.Title,
			Title:   subscription.Title,
			Type:    "rss",
			XMLURL:  subscription.URL,
			HTMLURL: stringValue(subscription.SiteURL),
		}
		if subscription.FolderID != nil {
			if folder := folderOutlines[*subscription.FolderID]; folder != nil {
				folder.Outlines = append(folder.Outlines, feedOutline)
				continue
			}
		}
		rootFeeds = append(rootFeeds, feedOutline)
	}
	outlines := make([]opmlOutline, 0, len(folderOutlines)+len(rootFeeds))
	for _, folder := range overview.Folders {
		if outline := folderOutlines[folder.ID]; outline != nil {
			outlines = append(outlines, *outline)
		}
	}
	outlines = append(outlines, rootFeeds...)
	document := opmlDocument{
		Version: "2.0",
		Head:    opmlHead{Title: "WebTag subscriptions", DateCreated: s.now().UTC().Format(time.RFC1123Z)},
		Body:    opmlBody{Outlines: outlines},
	}
	payload, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal subscriptions OPML: %w", err)
	}
	return append([]byte(xml.Header), payload...), nil
}

func (s *FeedService) ImportOPML(ctx context.Context, payload []byte) (model.OPMLImportResponse, error) {
	entries, err := decodeOPMLEntries(payload)
	if err != nil {
		return model.OPMLImportResponse{}, err
	}
	response := model.OPMLImportResponse{Errors: make([]string, 0)}
	normalized, err := normalizeOPMLEntries(entries, &response)
	if err != nil {
		return model.OPMLImportResponse{}, err
	}
	err = s.withMutations(ctx, opmlMutationKeys(normalized), func(lockCtx context.Context) error {
		s.importOPMLEntries(lockCtx, normalized, &response)
		return nil
	})
	if err != nil {
		return model.OPMLImportResponse{}, err
	}
	return response, nil
}

func decodeOPMLEntries(payload []byte) ([]opmlEntry, error) {
	var document opmlDocument
	if err := xml.Unmarshal(payload, &document); err != nil {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_opml", "invalid OPML document")
	}
	entries := make([]opmlEntry, 0)
	if err := collectOPMLEntries(document.Body.Outlines, &entries); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "empty_opml", "OPML document contains no subscriptions")
	}
	return entries, nil
}

func normalizeOPMLEntries(entries []opmlEntry, response *model.OPMLImportResponse) ([]opmlEntry, error) {
	normalized := make([]opmlEntry, 0, min(len(entries), maxOPMLSubscriptions))
	seenURLs := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		feedURL, err := feedremote.ValidateURL(entry.URL)
		if err != nil {
			response.Skipped++
			response.Errors = append(response.Errors, "skipped invalid feed URL")
			continue
		}
		if _, duplicate := seenURLs[feedURL]; duplicate {
			response.Skipped++
			response.Errors = append(response.Errors, "skipped duplicate feed URL")
			continue
		}
		seenURLs[feedURL] = struct{}{}
		title := strings.TrimSpace(entry.Title)
		if title == "" {
			title = feedURL
		}
		if utf8.RuneCountInString(title) > maxFeedTitleRunes {
			title = string([]rune(title)[:maxFeedTitleRunes])
		}
		normalized = append(normalized, opmlEntry{URL: feedURL, Title: title, Folder: entry.Folder})
		if len(normalized) > maxOPMLSubscriptions {
			return nil, httperr.NewWithCode(http.StatusRequestEntityTooLarge, "opml_subscription_limit", "OPML document exceeds 500 unique subscriptions")
		}
	}
	return normalized, nil
}

func opmlMutationKeys(entries []opmlEntry) []string {
	keys := make([]string, 0, len(entries)+1)
	keys = append(keys, "feed-opml-import")
	for _, entry := range entries {
		keys = append(keys, "feed-subscription:"+entry.URL)
	}
	return keys
}

func (s *FeedService) importOPMLEntries(ctx context.Context, entries []opmlEntry, response *model.OPMLImportResponse) {
	folders := make(map[string]uuid.UUID)
	for _, entry := range entries {
		var folderID *uuid.UUID
		// OPML is authoritative organization. A root-level outline explicitly
		// moves a restored/existing subscription into the ungrouped bucket.
		if entry.Folder != "" {
			id, ok := folders[entry.Folder]
			if !ok {
				folder, err := s.store.CreateFolder(ctx, entry.Folder)
				if err != nil {
					response.Skipped++
					response.Errors = append(response.Errors, "unable to create OPML folder")
					continue
				}
				id = folder.ID
				folders[entry.Folder] = id
				response.Folders++
			}
			folderID = &id
		}
		if _, err := s.store.CreateSubscription(ctx, entry.URL, folderID, true, entry.Title); err != nil {
			response.Skipped++
			response.Errors = append(response.Errors, "unable to import a subscription")
			continue
		}
		response.Imported++
	}
}

type opmlEntry struct {
	URL    string
	Title  string
	Folder string
}

func collectOPMLEntries(outlines []opmlOutline, entries *[]opmlEntry) error {
	type frame struct {
		outline opmlOutline
		parents []string
		depth   int
	}
	stack := make([]frame, 0, len(outlines))
	for i := len(outlines) - 1; i >= 0; i-- {
		stack = append(stack, frame{outline: outlines[i], depth: 1})
	}
	visited := 0
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		visited++
		if visited > maxOPMLOutlineNodes {
			return httperr.NewWithCode(http.StatusRequestEntityTooLarge, "opml_outline_limit", "OPML document contains too many outline nodes")
		}
		if current.depth > maxOPMLDepth {
			return httperr.NewWithCode(http.StatusUnprocessableEntity, "opml_depth_limit", "OPML outline nesting exceeds 32 levels")
		}
		name := strings.TrimSpace(current.outline.Title)
		if name == "" {
			name = strings.TrimSpace(current.outline.Text)
		}
		if strings.TrimSpace(current.outline.XMLURL) != "" {
			*entries = append(*entries, opmlEntry{URL: current.outline.XMLURL, Title: name, Folder: flattenOPMLFolder(current.parents)})
			continue
		}
		nextParents := current.parents
		if name != "" {
			nextParents = append(append([]string(nil), current.parents...), name)
		}
		for i := len(current.outline.Outlines) - 1; i >= 0; i-- {
			stack = append(stack, frame{outline: current.outline.Outlines[i], parents: nextParents, depth: current.depth + 1})
		}
	}
	return nil
}

func flattenOPMLFolder(parts []string) string {
	name := strings.Join(parts, " / ")
	if utf8.RuneCountInString(name) <= maxFeedFolderRunes {
		return name
	}
	return string([]rune(name)[:maxFeedFolderRunes])
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
