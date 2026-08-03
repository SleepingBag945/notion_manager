package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// UploadFileToNotion executes the full 5-step Notion file upload flow:
//
//	Step 1: getUploadFileUrlForAssistantChatTranscriptUpload → get presigned S3 URL
//	Step 2: POST file to S3 using presigned fields
//	Step 3: enqueueTask(processAgentAttachment) → server-side processing
//	Step 4: getTasks polling → wait for processing completion
//	Step 5: (optional) getSignedFileUrls → get accessible URL
//
// Returns an UploadedAttachment with the attachment:UUID:filename URL for transcript injection.
func UploadFileToNotion(acc *Account, file *FileAttachment) (*UploadedAttachment, error) {
	sessionID := generateUUIDv4()
	client := getChromeHTTPClient(30 * time.Second)

	// Generate UUID-based filename preserving original extension
	ext := filepath.Ext(file.FileName)
	if ext == "" {
		ext = mimeToExt(file.ContentType)
	}
	uuidFileName := generateUUIDv4() + ext

	// Step 1: Get presigned upload URL
	log.Printf("[upload-debug] Step 1: getUploadUrl for %s (%s, %d bytes)", file.FileName, file.ContentType, len(file.Data))
	uploadReq := NotionUploadURLRequest{
		Name:         uuidFileName,
		ContentType:  file.ContentType,
		ContentLen:   len(file.Data),
		CreateThread: true,
		Pointer: NotionAssistantChatPointer{
			SpaceID: acc.SpaceID,
			Table:   "thread",
			ID:      sessionID,
		},
	}

	uploadResp, err := notionAPICall[NotionUploadURLResponse](client, acc, "/getUploadFileUrlForAssistantChatTranscriptUpload", uploadReq)
	if err != nil {
		return nil, fmt.Errorf("step 1 getUploadUrl: %w", err)
	}
	log.Printf("[upload-debug] Step 1 OK: attachment URL = %s", uploadResp.URL)

	// Step 2: Upload file to S3
	log.Printf("[upload-debug] Step 2: S3 upload to %s", uploadResp.SignedUploadPostURL)
	err = uploadToS3(client, uploadResp.SignedUploadPostURL, uploadResp.Fields, file.Data, file.ContentType)
	if err != nil {
		return nil, fmt.Errorf("step 2 S3 upload: %w", err)
	}
	log.Printf("[upload-debug] Step 2 OK: S3 upload complete")

	// Step 3: Enqueue processing task
	log.Printf("[upload-debug] Step 3: enqueueTask(processAgentAttachment)")
	taskReq := NotionEnqueueTaskRequest{
		Task: NotionTask{
			EventName: "processAgentAttachment",
			Request: NotionTaskRequest{
				URL:     uploadResp.URL,
				SpaceID: acc.SpaceID,
				AISessionPointer: NotionAssistantChatPointer{
					SpaceID: acc.SpaceID,
					Table:   "thread",
					ID:      sessionID,
				},
				Source:        "user_upload",
				ClientVersion: acc.ClientVersion,
			},
			CellRouting: NotionCellRouting{
				SpaceIDs: []string{acc.SpaceID},
			},
		},
	}

	var enqueueResp struct {
		TaskID string `json:"taskId"`
	}
	enqueueRespRaw, err := notionAPICallRaw(client, acc, "/enqueueTask", taskReq)
	if err != nil {
		return nil, fmt.Errorf("step 3 enqueueTask: %w", err)
	}
	if err := json.Unmarshal(enqueueRespRaw, &enqueueResp); err != nil {
		return nil, fmt.Errorf("step 3 parse taskId: %w (body: %s)", err, string(enqueueRespRaw[:min(len(enqueueRespRaw), 200)]))
	}
	log.Printf("[upload-debug] Step 3 OK: taskId = %s", enqueueResp.TaskID)

	// Step 4: Poll task status and capture result metadata
	log.Printf("[upload-debug] Step 4: polling task status")
	taskResult, err := pollTaskCompletion(client, acc, enqueueResp.TaskID, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("step 4 poll task: %w", err)
	}
	log.Printf("[upload-debug] Step 4 OK: task completed")

	// Extract enriched metadata from task result if available.
	// Task result structure: status.result.data.stepMetadata contains the enriched metadata.
	var metadata *AttachmentMetadata
	if taskResult != nil {
		var taskData struct {
			Status struct {
				Result *struct {
					Type string `json:"type"`
					Data *struct {
						FileSizeBytes int64                  `json:"fileSizeBytes"`
						AiTraceId     string                 `json:"aiTraceId"`
						ContentType   string                 `json:"contentType"`
						Width         int                    `json:"width"`
						Height        int                    `json:"height"`
						Moderation    map[string]interface{} `json:"moderation"`
						StepMetadata  *struct {
							Guardrail       *AttachmentGuardrail   `json:"guardrail"`
							FileSizeBytes   int64                  `json:"fileSizeBytes"`
							AiTraceId       string                 `json:"aiTraceId"`
							EstimatedTokens map[string]interface{} `json:"estimatedTokens"`
							Width           int                    `json:"width"`
							Height          int                    `json:"height"`
							Moderation      map[string]interface{} `json:"moderation"`
							// CSV-specific fields
							NumRows          int    `json:"numRows"`
							NumFields        int    `json:"numFields"`
							TruncatedContent string `json:"truncatedContent"`
							WasTruncated     bool   `json:"wasTruncated"`
						} `json:"stepMetadata"`
					} `json:"data"`
				} `json:"result"`
			} `json:"status"`
		}
		if err := json.Unmarshal(taskResult, &taskData); err == nil && taskData.Status.Result != nil && taskData.Status.Result.Data != nil {
			data := taskData.Status.Result.Data
			metadata = &AttachmentMetadata{
				FileSizeBytes:    data.FileSizeBytes,
				AiTraceId:        data.AiTraceId,
				ContentType:      data.ContentType,
				Width:            data.Width,
				Height:           data.Height,
				Moderation:       data.Moderation,
				AttachmentSource: "user_upload",
				EstimatedTokens:  map[string]interface{}{"openai": 0, "anthropic": 0},
			}
			if sm := data.StepMetadata; sm != nil {
				metadata.Guardrail = sm.Guardrail
				if sm.EstimatedTokens != nil {
					metadata.EstimatedTokens = sm.EstimatedTokens
				}
				if sm.FileSizeBytes > 0 {
					metadata.FileSizeBytes = sm.FileSizeBytes
				}
				if sm.AiTraceId != "" {
					metadata.AiTraceId = sm.AiTraceId
				}
				// CSV-specific
				metadata.NumRows = sm.NumRows
				metadata.NumFields = sm.NumFields
				metadata.TruncatedContent = sm.TruncatedContent
				metadata.WasTruncated = sm.WasTruncated
			}
			log.Printf("[upload-debug] extracted task metadata: size=%d, tokens=%v, guardrail=%v, width=%d, height=%d",
				metadata.FileSizeBytes, metadata.EstimatedTokens, metadata.Guardrail, metadata.Width, metadata.Height)
		} else if err != nil {
			log.Printf("[upload-debug] failed to parse task result metadata: %v", err)
		}
	}

	return &UploadedAttachment{
		AttachmentURL: uploadResp.URL,
		FileName:      file.FileName,
		ContentType:   file.ContentType,
		FileSizeBytes: int64(len(file.Data)),
		SessionID:     sessionID,
		Metadata:      metadata,
	}, nil
}

// BuildAttachmentTranscript creates an attachment transcript entry from an uploaded file.
// Uses enriched metadata from the processAgentAttachment task if available.
func BuildAttachmentTranscript(uploaded *UploadedAttachment) AttachmentTranscriptMsg {
	// Use enriched metadata from task result, or construct defaults
	var meta AttachmentMetadata
	if uploaded.Metadata != nil {
		meta = *uploaded.Metadata
		// Ensure required fields are set
		if meta.AttachmentSource == "" {
			meta.AttachmentSource = "user_upload"
		}
		if meta.EstimatedTokens == nil {
			meta.EstimatedTokens = map[string]interface{}{"openai": 0, "anthropic": 0}
		}
	} else {
		meta = AttachmentMetadata{
			TruncatedContent: "",
			FileSizeBytes:    uploaded.FileSizeBytes,
			WasTruncated:     false,
			EstimatedTokens:  map[string]interface{}{"openai": 0, "anthropic": 0},
			AttachmentSource: "user_upload",
			AiTraceId:        generateUUIDv4(),
		}
	}

	return AttachmentTranscriptMsg{
		Type:        "attachment",
		FileUrl:     uploaded.AttachmentURL,
		FileName:    uploaded.FileName,
		ContentType: uploaded.ContentType,
		Metadata:    meta,
	}
}

// ── Internal helpers ──

// notionAPICall makes a POST to a Notion API endpoint with JSON body and returns parsed response.
func notionAPICall[T any](client *http.Client, acc *Account, endpoint string, body interface{}) (*T, error) {
	raw, err := notionAPICallRaw(client, acc, endpoint, body)
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w (body: %s)", err, string(raw[:min(len(raw), 300)]))
	}
	return &result, nil
}

// notionAPICallRaw makes a POST to a Notion API endpoint and returns raw response bytes.
func notionAPICallRaw(client *http.Client, acc *Account, endpoint string, body interface{}) ([]byte, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", NotionAPIBase+endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	setNotionHeadersJSON(req, acc)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 500)]))
	}
	return respBody, nil
}

// uploadToS3 uploads file data to S3 using presigned POST fields.
// Uses a plain http.Client (not Chrome TLS) because S3 presigned POST requires HTTP/1.1.
func uploadToS3(_ *http.Client, postURL string, fields map[string]string, data []byte, contentType string) error {
	s3Client := &http.Client{Timeout: 30 * time.Second}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Write all presigned fields first (order matters for S3)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			return fmt.Errorf("write field %s: %w", k, err)
		}
	}

	// Write the file as the last field
	part, err := writer.CreateFormFile("file", "upload")
	if err != nil {
		return fmt.Errorf("create file part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("write file data: %w", err)
	}
	writer.Close()

	req, err := http.NewRequest("POST", postURL, &buf)
	if err != nil {
		return fmt.Errorf("create S3 request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s3Client.Do(req)
	if err != nil {
		return fmt.Errorf("S3 upload request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 upload error %d: %s", resp.StatusCode, string(body[:min(len(body), 500)]))
	}
	return nil
}

// pollTaskCompletion polls getTasks until the task completes or times out.
// Returns the raw task result JSON on success (contains enriched metadata).
func pollTaskCompletion(client *http.Client, acc *Account, taskID string, timeout time.Duration) (json.RawMessage, error) {
	deadline := time.Now().Add(timeout)
	interval := 1 * time.Second

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		tasksReq := NotionGetTasksRequest{TaskIDs: []string{taskID}}
		raw, err := notionAPICallRaw(client, acc, "/getTasks", tasksReq)
		if err != nil {
			log.Printf("[upload-debug] poll error (will retry): %v", err)
			continue
		}

		// Parse with raw results to capture full task data
		var tasksResp struct {
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(raw, &tasksResp); err != nil {
			log.Printf("[upload-debug] poll parse error (will retry): %v", err)
			continue
		}

		for _, rawResult := range tasksResp.Results {
			var t struct {
				ID    string `json:"id"`
				State string `json:"state"`
			}
			json.Unmarshal(rawResult, &t)
			if t.ID == taskID || strings.HasPrefix(taskID, t.ID) || strings.HasPrefix(t.ID, taskID) {
				log.Printf("[upload-debug] task %s state: %s", taskID, t.State)
				if t.State == "success" || t.State == "completed" {
					return rawResult, nil
				}
				if t.State == "failure" || t.State == "failed" {
					return nil, fmt.Errorf("task failed: %s (result: %s)", t.State, string(rawResult[:min(len(rawResult), 500)]))
				}
			}
		}

		// Increase poll interval after first attempt
		if interval < 2*time.Second {
			interval = 2 * time.Second
		}
	}

	return nil, fmt.Errorf("task %s timed out after %v", taskID, timeout)
}

// mimeToExt maps common MIME types to file extensions.
func mimeToExt(mime string) string {
	switch strings.ToLower(mime) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "text/csv":
		return ".csv"
	case "text/plain":
		return ".txt"
	default:
		return ".bin"
	}
}
