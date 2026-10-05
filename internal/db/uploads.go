package db

import (
	"github.com/TheOutdoorProgrammer/crate/internal/models"
)

// Upload staging

func (q *Queries) CreateUploadFile(f *models.UploadFile) error {
	res, err := q.db.Exec(
		`INSERT INTO upload_files (batch_id, filename, staged_path, size, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		f.BatchID, f.Filename, f.StagedPath, f.Size, f.State, now(), now(),
	)
	if err != nil {
		return err
	}
	f.ID, _ = res.LastInsertId()
	return nil
}

func scanUploadFile(row interface{ Scan(...any) error }) (*models.UploadFile, error) {
	var f models.UploadFile
	var skip int
	err := row.Scan(&f.ID, &f.BatchID, &f.Filename, &f.StagedPath, &f.Size, &f.State,
		&f.Meta, &f.Match, &skip, &f.Error, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	f.Skip = skip != 0
	return &f, nil
}

const uploadFileCols = `id, batch_id, filename, staged_path, size, state, meta_json, match_json, skip, error, created_at, updated_at`

func (q *Queries) ListUploadFiles(batchID string) ([]models.UploadFile, error) {
	rows, err := q.db.Query(
		`SELECT `+uploadFileCols+` FROM upload_files WHERE batch_id = ? ORDER BY id`, batchID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []models.UploadFile
	for rows.Next() {
		f, err := scanUploadFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	return files, rows.Err()
}

func (q *Queries) GetUploadFile(id int64) (*models.UploadFile, error) {
	return scanUploadFile(q.db.QueryRow(
		`SELECT `+uploadFileCols+` FROM upload_files WHERE id = ?`, id,
	))
}

// UpdateUploadFileResult records identification output: decoded meta, proposed
// match, resulting state, and an optional error/reason string.
func (q *Queries) UpdateUploadFileResult(id int64, state string, metaJSON, matchJSON, errStr *string) error {
	_, err := q.db.Exec(
		`UPDATE upload_files SET state = ?, meta_json = ?, match_json = ?, error = ?, updated_at = ? WHERE id = ?`,
		state, metaJSON, matchJSON, errStr, now(), id,
	)
	return err
}

// UpdateUploadFileMatch overwrites the proposed match (user override) and
// resets state to identified when a track is assigned.
func (q *Queries) UpdateUploadFileMatch(id int64, matchJSON *string, state string) error {
	_, err := q.db.Exec(
		`UPDATE upload_files SET match_json = ?, state = ?, updated_at = ? WHERE id = ?`,
		matchJSON, state, now(), id,
	)
	return err
}

func (q *Queries) SetUploadFileSkip(id int64, skip bool) error {
	s := 0
	if skip {
		s = 1
	}
	_, err := q.db.Exec(
		`UPDATE upload_files SET skip = ?, updated_at = ? WHERE id = ?`, s, now(), id,
	)
	return err
}

func (q *Queries) UpdateUploadFileState(id int64, state string, errStr *string) error {
	_, err := q.db.Exec(
		`UPDATE upload_files SET state = ?, error = ?, updated_at = ? WHERE id = ?`,
		state, errStr, now(), id,
	)
	return err
}

// DeleteQueuedForTrack removes non-active queue rows for a track after an
// upload claims it — active rows (searching/downloading/organizing) belong to
// the downloader and are left alone.
func (q *Queries) DeleteQueuedForTrack(trackID int64) error {
	_, err := q.db.Exec(
		`DELETE FROM download_queue WHERE track_id = ? AND status IN ('pending', 'failed', 'complete')`,
		trackID,
	)
	return err
}

func (q *Queries) DeleteUploadBatch(batchID string) error {
	_, err := q.db.Exec(`DELETE FROM upload_files WHERE batch_id = ?`, batchID)
	return err
}

// ListUploadBatches returns batch ids with file counts, newest first.
func (q *Queries) ListUploadBatches() ([]models.UploadBatchSummary, error) {
	rows, err := q.db.Query(
		`SELECT batch_id, COUNT(*), MIN(created_at),
		        SUM(CASE WHEN state = 'identified' AND skip = 0 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN state = 'unidentified' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN state = 'committed' THEN 1 ELSE 0 END)
		 FROM upload_files GROUP BY batch_id ORDER BY MIN(created_at) DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var batches []models.UploadBatchSummary
	for rows.Next() {
		var b models.UploadBatchSummary
		if err := rows.Scan(&b.BatchID, &b.Total, &b.CreatedAt, &b.Identified, &b.Unidentified, &b.Committed); err != nil {
			return nil, err
		}
		batches = append(batches, b)
	}
	return batches, rows.Err()
}
