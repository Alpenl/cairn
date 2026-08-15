package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"time"

	"webtag/internal/httperr"
	"webtag/internal/repository"
)

const (
	ArchiveV2SectionBase     = "base"
	ArchiveV2SectionThoughts = "thoughts"
	ArchiveV2SectionNotes    = "notes"
)

// ArchiveV2Selection is the closed selector set accepted by the v2 archive
// endpoint. Base is mandatory and represented implicitly, so the zero value
// is the explicit `base` selection.
type ArchiveV2Selection struct {
	IncludeThoughts bool
	IncludeNotes    bool
}

// FullArchiveV2Selection is the omission-compatible archive selector. Reader
// requests still send its equivalent canonical query value explicitly.
func FullArchiveV2Selection() ArchiveV2Selection {
	return ArchiveV2Selection{IncludeThoughts: true, IncludeNotes: true}
}

func (s ArchiveV2Selection) Tokens() []string {
	tokens := []string{ArchiveV2SectionBase}
	if s.IncludeThoughts {
		tokens = append(tokens, ArchiveV2SectionThoughts)
	}
	if s.IncludeNotes {
		tokens = append(tokens, ArchiveV2SectionNotes)
	}
	return tokens
}

func (s ArchiveV2Selection) Canonical() string {
	return strings.Join(s.Tokens(), ",")
}

// ParseArchiveV2Sections accepts only the four canonical query values. It
// deliberately does not trim, lowercase, sort, or deduplicate input: accepting
// a near miss would make the wire contract ambiguous and hide client bugs.
func ParseArchiveV2Sections(values []string, present bool) (ArchiveV2Selection, error) {
	if !present {
		return FullArchiveV2Selection(), nil
	}
	if len(values) != 1 {
		return ArchiveV2Selection{}, invalidArchiveV2Sections()
	}
	switch values[0] {
	case ArchiveV2SectionBase:
		return ArchiveV2Selection{}, nil
	case ArchiveV2SectionBase + "," + ArchiveV2SectionThoughts:
		return ArchiveV2Selection{IncludeThoughts: true}, nil
	case ArchiveV2SectionBase + "," + ArchiveV2SectionNotes:
		return ArchiveV2Selection{IncludeNotes: true}, nil
	case ArchiveV2SectionBase + "," + ArchiveV2SectionThoughts + "," + ArchiveV2SectionNotes:
		return FullArchiveV2Selection(), nil
	default:
		return ArchiveV2Selection{}, invalidArchiveV2Sections()
	}
}

func invalidArchiveV2Sections() error {
	return httperr.NewWithCode(
		http.StatusUnprocessableEntity,
		httperr.CodeInvalidArchiveSections,
		"sections must be one of base, base,thoughts, base,notes, or base,thoughts,notes",
	)
}

// ArchiveV2ExportOptions is captured by the HTTP handler before it begins the
// stream. The namespace is the exact authenticated representation marker that
// the Reader must validate against the response header and active lease.
type ArchiveV2ExportOptions struct {
	Selection           ArchiveV2Selection
	ClientDataNamespace string
}

type archiveV2Manifest struct {
	ClientDataNamespace string         `json:"client_data_namespace"`
	Sections            []string       `json:"sections"`
	Counts              map[string]int `json:"counts"`
	ChecksumSHA256      string         `json:"checksum_sha256"`
}

// ArchiveV2Service streams a versioned, relationship-preserving archive. Links
// keep the established DTO export path; site and Reader relations stream from
// their dedicated repositories, so no section is fully buffered.
type ArchiveV2Service struct {
	links interface {
		Export(context.Context, io.Writer) error
		ExportWithCount(context.Context, io.Writer) (int, error)
	}
	archive repository.ArchiveV2Reader
	rules   repository.ArchiveV2RuleReader
	reader  ReaderArchiveExporter
}

func NewArchiveV2Service(links interface {
	Export(context.Context, io.Writer) error
	ExportWithCount(context.Context, io.Writer) (int, error)
}, archive repository.ArchiveV2Reader, rules repository.ArchiveV2RuleReader) *ArchiveV2Service {
	return &ArchiveV2Service{links: links, archive: archive, rules: rules}
}

// WithReaderArchive attaches the Reader-owned rows when its schema is ready.
func (s *ArchiveV2Service) WithReaderArchive(reader ReaderArchiveExporter) *ArchiveV2Service {
	s.reader = reader
	return s
}

// Export keeps the pre-selector complete archive stream for internal callers
// that do not need a browser-verifiable manifest. The public endpoint uses
// ExportSelected below.
func (s *ArchiveV2Service) Export(ctx context.Context, w io.Writer) error {
	return s.export(ctx, w, FullArchiveV2Selection(), "", false)
}

// ValidateSelection checks whether the selected private Reader groups can be
// exported before an HTTP handler commits a streaming success response.
func (s *ArchiveV2Service) ValidateSelection(selection ArchiveV2Selection) error {
	if s.reader == nil && (selection.IncludeThoughts || selection.IncludeNotes) {
		return httperr.NewWithCode(
			http.StatusServiceUnavailable,
			httperr.CodeArchiveReaderUnavailable,
			"selected Reader archive sections are unavailable",
		)
	}
	return nil
}

// ExportSelected streams only the requested archive groups and appends a
// manifest with exact array counts and a SHA-256 over the verbatim JSON bytes
// that precede `,"manifest":`. The browser validates that same byte prefix
// before constructing a downloadable Blob.
func (s *ArchiveV2Service) ExportSelected(ctx context.Context, w io.Writer, options ArchiveV2ExportOptions) error {
	if options.ClientDataNamespace == "" {
		return fmt.Errorf("archive v2 requires an authenticated client data namespace")
	}
	if err := s.ValidateSelection(options.Selection); err != nil {
		return err
	}
	return s.export(ctx, w, options.Selection, options.ClientDataNamespace, true)
}

func (s *ArchiveV2Service) export(
	ctx context.Context,
	w io.Writer,
	selection ArchiveV2Selection,
	clientDataNamespace string,
	includeManifest bool,
) error {
	payloadWriter := w
	var payloadHash hash.Hash
	if includeManifest {
		payloadHash = sha256.New()
		payloadWriter = io.MultiWriter(w, payloadHash)
	}
	if err := writeArchiveV2Preamble(payloadWriter); err != nil {
		return err
	}
	counts, err := s.writeArchiveV2Sections(ctx, payloadWriter, selection, includeManifest)
	if err != nil {
		return err
	}
	if includeManifest {
		if err := writeArchiveV2Manifest(w, payloadHash, clientDataNamespace, selection, counts); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "}")
	return err
}

func writeArchiveV2Preamble(w io.Writer) error {
	_, err := fmt.Fprintf(w, `{"schema_version":2,"exported_at":%q,"generator_version":"webtag","links":`, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *ArchiveV2Service) writeArchiveV2Sections(ctx context.Context, w io.Writer, selection ArchiveV2Selection, includeCounts bool) (map[string]int, error) {
	counts := make(map[string]int, 6+len(readerArchiveSections))
	if err := s.writeArchiveV2Links(ctx, w, includeCounts, counts); err != nil {
		return nil, err
	}
	if err := s.writeArchiveV2SiteSections(ctx, w, includeCounts, counts); err != nil {
		return nil, err
	}
	if err := s.writeArchiveV2ClassificationRules(ctx, w, includeCounts, counts); err != nil {
		return nil, err
	}
	if err := s.writeArchiveV2Reader(ctx, w, selection, includeCounts, counts); err != nil {
		return nil, err
	}
	return counts, nil
}

func (s *ArchiveV2Service) writeArchiveV2Links(ctx context.Context, w io.Writer, includeCounts bool, counts map[string]int) error {
	if !includeCounts {
		return s.links.Export(ctx, w)
	}
	count, err := s.links.ExportWithCount(ctx, w)
	if err == nil {
		counts["links"] = count
	}
	return err
}

func (s *ArchiveV2Service) writeArchiveV2SiteSections(ctx context.Context, w io.Writer, includeCounts bool, counts map[string]int) error {
	for _, section := range []string{"sites", "site_entries", "site_tags", "site_identities"} {
		count, err := writeArchiveV2Array(w, `,"`+section+`":[`, func(yield func([]byte) error) error {
			return s.archive.StreamArchiveV2Section(ctx, section, yield)
		})
		if err != nil {
			return err
		}
		if includeCounts {
			counts[section] = count
		}
	}
	return nil
}

func (s *ArchiveV2Service) writeArchiveV2ClassificationRules(ctx context.Context, w io.Writer, includeCounts bool, counts map[string]int) error {
	count, err := writeArchiveV2Array(w, `,"classification_rules":[`, func(yield func([]byte) error) error {
		return s.rules.StreamArchiveV2Rules(ctx, yield)
	})
	if err == nil && includeCounts {
		counts["classification_rules"] = count
	}
	return err
}

func (s *ArchiveV2Service) writeArchiveV2Reader(ctx context.Context, w io.Writer, selection ArchiveV2Selection, includeCounts bool, counts map[string]int) error {
	if s.reader == nil {
		return nil
	}
	if _, err := io.WriteString(w, `,"reader":`); err != nil {
		return err
	}
	readerCounts, err := s.reader.ExportReaderArchive(ctx, w, selection)
	if err != nil {
		return err
	}
	if includeCounts {
		for section, count := range readerCounts {
			counts["reader."+section] = count
		}
	}
	return nil
}

func writeArchiveV2Manifest(w io.Writer, payloadHash hash.Hash, clientDataNamespace string, selection ArchiveV2Selection, counts map[string]int) error {
	manifestBytes, err := json.Marshal(archiveV2Manifest{
		ClientDataNamespace: clientDataNamespace,
		Sections:            selection.Tokens(),
		Counts:              counts,
		ChecksumSHA256:      hex.EncodeToString(payloadHash.Sum(nil)),
	})
	if err != nil {
		return fmt.Errorf("marshal archive v2 manifest: %w", err)
	}
	if _, err := io.WriteString(w, `,"manifest":`); err != nil {
		return err
	}
	_, err = w.Write(manifestBytes)
	return err
}

// writeArchiveV2Array emits one streamed section as a JSON array and returns
// its row count. Rows are relayed as the repository yields them, so counting
// does not add a second query or make memory grow with archive size.
func writeArchiveV2Array(w io.Writer, prefix string, stream func(yield func(raw []byte) error) error) (int, error) {
	if _, err := io.WriteString(w, prefix); err != nil {
		return 0, err
	}
	count := 0
	if err := stream(func(raw []byte) error {
		if count > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
		count++
		return nil
	}); err != nil {
		return 0, err
	}
	if _, err := io.WriteString(w, "]"); err != nil {
		return 0, err
	}
	return count, nil
}
