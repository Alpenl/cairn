package dto

import "time"

// ConceptExportItem is one installation vocabulary entry streamed by
// GET /api/export/concepts. It deliberately exposes only portable fields and
// omits internal embedding data.
type ConceptExportItem struct {
	ID          string    `json:"id"`
	PrimaryName string    `json:"primary_name"`
	DisplayName string    `json:"display_name"`
	Aliases     []string  `json:"aliases"`
	UseCount    int       `json:"use_count"`
	CreatedAt   time.Time `json:"created_at"`
}
