package proxy

import "strings"

const (
	largeSystemAttachmentThresholdBytes = 16 * 1024
	largeSystemAttachmentFileName       = "system-and-developer-instructions.txt"
	largeSystemAttachmentBootstrap      = "The complete system and developer instructions are in the attached file \"system-and-developer-instructions.txt\". Read that file first and follow it exactly before answering the user."
)

func isSystemInstructionRole(role string) bool {
	return role == "system" || role == "developer"
}

func buildLargeSystemAttachment(messages []ChatMessage, isFirstTurn bool) *FileAttachment {
	if !isFirstTurn {
		return nil
	}

	var systemParts []string
	for _, message := range messages {
		if isSystemInstructionRole(message.Role) {
			systemParts = append(systemParts, message.Content)
		}
	}

	mergedSystem := strings.Join(systemParts, "\n\n")
	if len(mergedSystem) < largeSystemAttachmentThresholdBytes {
		return nil
	}

	return &FileAttachment{
		Data:        []byte(mergedSystem),
		FileName:    largeSystemAttachmentFileName,
		ContentType: "text/plain",
	}
}

func replaceSystemMessagesWithAttachmentBootstrap(messages []ChatMessage) []ChatMessage {
	result := make([]ChatMessage, 0, len(messages)+1)
	for _, message := range messages {
		if isSystemInstructionRole(message.Role) {
			continue
		}
		result = append(result, message)
	}
	return ensureLargeSystemAttachmentBootstrap(result)
}

func ensureLargeSystemAttachmentBootstrap(messages []ChatMessage) []ChatMessage {
	for _, message := range messages {
		if strings.Contains(message.Content, largeSystemAttachmentBootstrap) {
			return messages
		}
	}

	result := make([]ChatMessage, 0, len(messages)+1)
	result = append(result, ChatMessage{
		Role:    "system",
		Content: largeSystemAttachmentBootstrap,
	})
	result = append(result, messages...)
	return result
}
