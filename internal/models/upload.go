package models

// Upload states for staged upload files.
const (
	UploadStateUploaded     = "uploaded"
	UploadStateIdentified   = "identified"
	UploadStateUnidentified = "unidentified"
	UploadStateCommitted    = "committed"
	UploadStateSkipped      = "skipped"
	UploadStateFailed       = "failed"
)

// UploadBatchSummary aggregates one upload batch for the list endpoint.
type UploadBatchSummary struct {
	BatchID      string `json:"batch_id"`
	Total        int    `json:"total"`
	Identified   int    `json:"identified"`
	Unidentified int    `json:"unidentified"`
	Committed    int    `json:"committed"`
	CreatedAt    string `json:"created_at"`
}

// UploadFile is one staged upload. Meta and Match are raw JSON blobs decoded
// by the upload service; staged_path is the on-disk location under the upload
// dir and is never exposed to the API.
type UploadFile struct {
	ID         int64   `json:"id"`
	BatchID    string  `json:"batch_id"`
	Filename   string  `json:"filename"`
	StagedPath string  `json:"-"`
	Size       int64   `json:"size"`
	State      string  `json:"state"`
	Meta       *string `json:"-"`
	Match      *string `json:"-"`
	Skip       bool    `json:"skip"`
	Error      *string `json:"error,omitempty"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"-"`
}
