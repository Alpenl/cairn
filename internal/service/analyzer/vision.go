package analyzer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	visionImageTimeout   = 10 * time.Second
	visionImageMaxBytes  = 5 << 20
	visionImageMaxPixels = 24_000_000
)

// ErrVisionImageRejected is deliberately URL-free: image locators can carry
// signed queries and must not escape through logs or persisted parse errors.
var ErrVisionImageRejected = errors.New("vision image rejected by secure fetch policy")

func (a *OpenAIAnalyzer) prepareVisionImages(ctx context.Context, locators []string) ([]string, error) {
	if len(locators) == 0 {
		return nil, nil
	}
	if a == nil || a.visionClient == nil {
		return nil, ErrVisionImageRejected
	}

	prepared := make([]string, 0, min(len(locators), maxAnalysisImages))
	for _, raw := range locators {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if len(prepared) == maxAnalysisImages {
			break
		}

		var data []byte
		var err error
		if strings.HasPrefix(strings.ToLower(raw), "data:") {
			data, err = decodeVisionDataURL(raw)
		} else {
			data, err = a.fetchVisionImage(ctx, raw)
		}
		if err != nil {
			return nil, err
		}
		mediaType, err := validateVisionImage(data)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, "data:"+mediaType+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return prepared, nil
}

func (a *OpenAIAnalyzer) fetchVisionImage(ctx context.Context, raw string) ([]byte, error) {
	target, ok := parseVisionRemoteURL(raw)
	if !ok {
		return nil, ErrVisionImageRejected
	}

	req, cancel, err := a.visionClient.NewRequest(ctx, visionImageTimeout, http.MethodGet, target, nil)
	if err != nil {
		return nil, ErrVisionImageRejected
	}
	defer cancel()
	req.Header.Set("Accept", "image/png,image/jpeg")

	resp, err := a.visionClient.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrVisionImageRejected, ctxErr)
		}
		return nil, ErrVisionImageRejected
	}
	defer resp.Body.Close()
	return readVisionImageResponse(resp)
}

func parseVisionRemoteURL(raw string) (string, bool) {
	target, err := url.Parse(raw)
	if err != nil || target == nil || target.Host == "" || target.User != nil {
		return "", false
	}
	if !strings.EqualFold(target.Scheme, "http") && !strings.EqualFold(target.Scheme, "https") {
		return "", false
	}
	return target.String(), true
}

func readVisionImageResponse(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, ErrVisionImageRejected
	}
	declared := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if declared != "" && declared != "application/octet-stream" && declared != "image/png" && declared != "image/jpeg" {
		return nil, ErrVisionImageRejected
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, visionImageMaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > visionImageMaxBytes {
		return nil, ErrVisionImageRejected
	}
	actual := http.DetectContentType(data)
	if declared != "" && declared != "application/octet-stream" && declared != actual {
		return nil, ErrVisionImageRejected
	}
	return data, nil
}

func decodeVisionDataURL(raw string) ([]byte, error) {
	comma := strings.IndexByte(raw, ',')
	if comma <= len("data:") || comma >= len(raw)-1 || comma > 128 {
		return nil, ErrVisionImageRejected
	}
	metadata := strings.ToLower(raw[len("data:"):comma])
	parts := strings.Split(metadata, ";")
	if len(parts) != 2 || (parts[0] != "image/png" && parts[0] != "image/jpeg") || parts[1] != "base64" {
		return nil, ErrVisionImageRejected
	}
	encoded := raw[comma+1:]
	if int64(len(encoded)) > int64(base64.StdEncoding.EncodedLen(visionImageMaxBytes)) {
		return nil, ErrVisionImageRejected
	}
	data, err := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)), visionImageMaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > visionImageMaxBytes {
		return nil, ErrVisionImageRejected
	}
	return data, nil
}

func validateVisionImage(data []byte) (string, error) {
	mediaType := http.DetectContentType(data)
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		// GIF is intentionally rejected even when static: accepting it requires
		// decoding the full frame table, which reintroduces an attacker-controlled
		// allocation before the frame budget can be enforced.
		return "", ErrVisionImageRejected
	}
	if mediaType == "image/png" && !isStaticPNG(data) {
		return "", ErrVisionImageRejected
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 ||
		(format != "png" && format != "jpeg") ||
		int64(config.Width) > visionImageMaxPixels/int64(config.Height) {
		return "", ErrVisionImageRejected
	}
	return mediaType, nil
}

func isStaticPNG(data []byte) bool { //nolint:gocyclo // linear chunk grammar validation keeps all container invariants adjacent
	const (
		pngSignatureSize = 8
		pngChunkOverhead = 12
	)
	if len(data) < pngSignatureSize || !bytes.Equal(data[:pngSignatureSize], []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}

	seenIHDR := false
	seenIDAT := false
	for offset := pngSignatureSize; offset <= len(data)-pngChunkOverhead; {
		declaredLength := binary.BigEndian.Uint32(data[offset : offset+4])
		if declaredLength > visionImageMaxBytes {
			return false
		}
		length := int(declaredLength) //nolint:gosec // bounded above by the 5 MiB vision limit on every architecture
		remaining := len(data) - offset - pngChunkOverhead
		if length > remaining {
			return false
		}
		chunkEnd := offset + 8 + length
		chunkType := string(data[offset+4 : offset+8])
		wantCRC := binary.BigEndian.Uint32(data[chunkEnd : chunkEnd+4])
		if crc32.ChecksumIEEE(data[offset+4:chunkEnd]) != wantCRC {
			return false
		}

		switch chunkType {
		case "IHDR":
			if seenIHDR || offset != pngSignatureSize || length != 13 {
				return false
			}
			seenIHDR = true
		case "IDAT":
			if !seenIHDR {
				return false
			}
			seenIDAT = true
		case "acTL", "fcTL", "fdAT":
			return false
		case "IEND":
			return seenIHDR && seenIDAT && length == 0 && chunkEnd+4 == len(data)
		}
		offset = chunkEnd + 4
	}
	return false
}
