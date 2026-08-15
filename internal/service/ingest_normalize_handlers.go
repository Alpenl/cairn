package service

import (
	"net/http"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/httperr"
)

func handleURLSource(acc *ingestAccumulator, src dto.IngestSource, record *ingestSourceRecord) error {
	rawURL, err := validateURL(src.URL)
	if err != nil {
		return err
	}
	record.URL = rawURL
	acc.noteSubmittedURL(src.URL)
	acc.noteRealURL(rawURL)
	return nil
}

func handleTextSource(acc *ingestAccumulator, src dto.IngestSource, record *ingestSourceRecord) error {
	text := strings.TrimSpace(src.Text)
	if text == "" {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeIngestTextRequired, "text source requires non-empty text")
	}
	record.Text = text
	acc.textParts = append(acc.textParts, text)
	return nil
}

func handleImageSource(acc *ingestAccumulator, src dto.IngestSource, record *ingestSourceRecord) error {
	imageURL, err := validateImageLocator(src.URL)
	if err != nil {
		return err
	}
	record.URL = imageURL
	acc.addImageOnce(imageURL)
	return nil
}

//nolint:gocyclo // reason: 浏览器捕获源的字段归一化是按字段类型逐个 if 处理的线性流程；每个 if 对应一种 ingest 字段（URL / image / text / metadata），独立无共享状态。
func handleBrowserCaptureSource(acc *ingestAccumulator, src dto.IngestSource, record *ingestSourceRecord) error {
	pageURL := strings.TrimSpace(src.URL)
	if pageURL != "" {
		validatedURL, err := validateURL(pageURL)
		if err != nil {
			return err
		}
		acc.noteSubmittedURL(pageURL)
		submittedURL := pageURL
		pageURL = validatedURL
		record.URL = pageURL
		acc.noteCaptureURL(pageURL, submittedURL)
	}

	title := strings.TrimSpace(src.Title)
	text := strings.TrimSpace(src.Text)
	html := strings.TrimSpace(src.HTML)
	record.Title = title
	record.Text = text
	record.HTML = html

	if len(src.Metadata) > 0 {
		if err := validateIngestMetadata(src.Metadata, "browser_capture metadata"); err != nil {
			return err
		}
		record.Metadata = cloneMap(src.Metadata)
	}

	hasMeaningfulField := pageURL != "" || title != "" || text != "" || html != "" || len(record.Metadata) > 0
	if !hasMeaningfulField {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeIngestBrowserCaptureEmpty, "browser_capture source requires at least one meaningful field")
	}

	if acc.firstTitle == "" && title != "" {
		acc.firstTitle = title
	}
	if text != "" {
		acc.textParts = append(acc.textParts, text)
	}
	if html != "" {
		acc.htmlParts = append(acc.htmlParts, html)
	}
	if len(record.Metadata) > 0 {
		acc.metadata["browser_capture"] = record.Metadata
		if acc.userNote == "" {
			if note, ok := record.Metadata["note"].(string); ok {
				acc.userNote = strings.TrimSpace(note)
			}
		}
	}
	return nil
}
