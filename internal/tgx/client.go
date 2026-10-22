// Package tgx 是极简 Telegram Bot API 客户端，只实现图床所需能力：
// 发送文档（密文分片）、下载文件、删除消息。
// 标准 Bot API 限制：上传 ≤50MB，getFile 下载 ≤20MB（分片大小必须兼容两者）。
package tgx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	apiBase   string
	token     string
	channelID string
	hc        *http.Client
}

func New(apiBase, token, channelID string) *Client {
	return &Client{
		apiBase:   strings.TrimRight(apiBase, "/"),
		token:     token,
		channelID: channelID,
		hc:        &http.Client{Timeout: 10 * time.Minute},
	}
}

// SetHTTPClient sets the storage transport for all API and file download requests.
func (c *Client) SetHTTPClient(hc *http.Client) { c.hc = hc }

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Result      json.RawMessage `json:"result"`
}

type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("tg %s 失败(%d): %s", e.Method, e.Code, e.Description)
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

func (c *Client) redact(message string) string {
	if c.token == "" {
		return message
	}
	for _, secret := range []string{c.token, url.QueryEscape(c.token), url.PathEscape(c.token)} {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	return message
}

func (c *Client) safeError(method string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	return &redactedError{message: "tg " + method + ": " + c.redact(err.Error()), cause: err}
}

func (c *Client) call(ctx context.Context, method string, form url.Values) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+"/bot"+c.token+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, c.safeError(method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req, method)
}

func (c *Client) do(req *http.Request, method string) (*apiResponse, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.safeError(method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, c.safeError(method, err)
	}
	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("tg %s 返回非 JSON（HTTP %d）", method, resp.StatusCode)
	}
	if !apiResp.OK {
		return nil, &APIError{Method: method, Code: apiResp.ErrorCode, Description: c.redact(apiResp.Description)}
	}
	return &apiResp, nil
}

// TestConnectivity 校验 token 与频道可达。
func (c *Client) TestConnectivity(ctx context.Context) error {
	if _, err := c.call(ctx, "getMe", url.Values{}); err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	if _, err := c.call(ctx, "getChat", url.Values{"chat_id": {c.channelID}}); err != nil {
		return fmt.Errorf("getChat(频道不可达或 Bot 无权限): %w", err)
	}
	return nil
}

type documentResult struct {
	MessageID int64 `json:"message_id"`
	Document  struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
	} `json:"document"`
}

// SendDocument 以文档形式发送一段密文到私有频道，返回消息定位信息。
func (c *Client) SendDocument(ctx context.Context, fileName string, data []byte, caption string) (messageID int64, fileID string, size int64, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", c.channelID)
	_ = w.WriteField("caption", caption)
	_ = w.WriteField("disable_content_type_detection", "true")
	fw, err := w.CreateFormFile("document", fileName)
	if err != nil {
		return 0, "", 0, err
	}
	if _, err := fw.Write(data); err != nil {
		return 0, "", 0, err
	}
	if err := w.Close(); err != nil {
		return 0, "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+"/bot"+c.token+"/sendDocument", &buf)
	if err != nil {
		return 0, "", 0, c.safeError("sendDocument", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.do(req, "sendDocument")
	if err != nil {
		return 0, "", 0, err
	}
	var dr documentResult
	if err := json.Unmarshal(resp.Result, &dr); err != nil {
		return 0, "", 0, fmt.Errorf("解析 sendDocument 响应: %w", err)
	}
	if dr.MessageID == 0 || dr.Document.FileID == "" {
		return 0, "", 0, errors.New("sendDocument 响应缺少 message_id/file_id")
	}
	return dr.MessageID, dr.Document.FileID, dr.Document.FileSize, nil
}

type fileResult struct {
	FilePath string `json:"file_path"`
	FileSize int64  `json:"file_size"`
}

// GetFileURL 返回文件的下载 URL（getFile + 拼接）。
func (c *Client) GetFileURL(ctx context.Context, fileID string) (string, error) {
	resp, err := c.call(ctx, "getFile", url.Values{"file_id": {fileID}})
	if err != nil {
		return "", err
	}
	var fr fileResult
	if err := json.Unmarshal(resp.Result, &fr); err != nil {
		return "", err
	}
	if fr.FilePath == "" {
		return "", errors.New("getFile 未返回 file_path")
	}
	return c.apiBase + "/file/bot" + c.token + "/" + fr.FilePath, nil
}

// Download 流式下载文件内容。
func (c *Client) Download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	u, err := c.GetFileURL(ctx, fileID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, c.safeError("download", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.safeError("download", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("下载 TG 文件失败: HTTP %d", resp.StatusCode)
	}
	return &safeBody{ReadCloser: resp.Body, client: c}, nil
}

type safeBody struct {
	io.ReadCloser
	client *Client
}

func (b *safeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	return n, b.client.safeError("download", err)
}
func (b *safeBody) Close() error { return b.client.safeError("download", b.ReadCloser.Close()) }

// DeleteMessage 删除频道消息（彻底删除原件时使用）。
func (c *Client) DeleteMessage(ctx context.Context, messageID int64) error {
	_, err := c.call(ctx, "deleteMessage", url.Values{
		"chat_id":    {c.channelID},
		"message_id": {strconv.FormatInt(messageID, 10)},
	})
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == 400 && strings.Contains(strings.ToLower(apiErr.Description), "message to delete not found") {
		return nil // The desired deletion is already complete, including retries.
	}
	return err
}
