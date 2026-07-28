package proxy

import (
	"fmt"
	"net/http"
	"strings"
)

// publicAPIError is the stable, user-facing error representation shared by
// the Anthropic and OpenAI compatible endpoints. Codes are intentionally
// protocol-independent so operators can search logs and documentation by ID.
type publicAPIError struct {
	Code    string
	Message string
}

func classifyPublicAPIError(status int, message, errType string) publicAPIError {
	text := strings.ToLower(strings.TrimSpace(message))

	switch {
	case status == http.StatusMethodNotAllowed:
		return publicAPIError{"NM1001", "请求方法不受支持"}
	case strings.Contains(text, "request body is required") || strings.Contains(text, "empty request body"):
		return publicAPIError{"NM1002", "请求体不能为空"}
	case status == http.StatusBadRequest && (strings.Contains(text, "invalid request") || strings.Contains(text, "failed to read request") || errType == "invalid_request_error"):
		return publicAPIError{"NM1003", "请求参数或 JSON 格式无效"}
	case strings.Contains(text, "all accounts busy") || strings.Contains(text, "concurrency") && status == http.StatusTooManyRequests:
		return publicAPIError{"NM2001", "所有可用账号均已达到并发上限，请稍后重试"}
	case strings.Contains(text, "no available account") || strings.Contains(text, "no account") || strings.Contains(text, "all accounts exhausted") || strings.Contains(text, "usage limit exceeded") || strings.Contains(text, "quota exhausted"):
		return publicAPIError{"NM2002", "暂无可用账号或账号额度已耗尽"}
	case strings.Contains(text, "empty response"):
		return publicAPIError{"NM3001", "上游返回了空响应，请稍后重试"}
	case status == http.StatusGatewayTimeout || strings.Contains(text, "timeout") || strings.Contains(text, "deadline exceeded"):
		return publicAPIError{"NM3002", "Notion 上游请求超时，请稍后重试"}
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || strings.Contains(text, "notion api") || strings.Contains(text, "upstream"):
		return publicAPIError{"NM3003", "Notion 上游服务异常，请稍后重试"}
	case status == http.StatusUnauthorized:
		return publicAPIError{"NM1004", "鉴权失败，请检查 API 密钥"}
	case status == http.StatusNotFound:
		return publicAPIError{"NM1005", "请求的接口或资源不存在"}
	default:
		return publicAPIError{"NM9000", "服务处理请求失败，请稍后重试"}
	}
}

func formatPublicAPIError(status int, message, errType string) publicAPIError {
	e := classifyPublicAPIError(status, message, errType)
	e.Message = fmt.Sprintf("[%s] %s", e.Code, e.Message)
	return e
}

func publicAPIErrorType(original string, status int) string {
	if original != "" {
		return original
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}
