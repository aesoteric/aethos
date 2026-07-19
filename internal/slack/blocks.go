package slack

const slackSectionTextLimit = 3000

type messageBlock struct {
	Type     string         `json:"type"`
	Text     *textObject    `json:"text,omitempty"`
	Elements []blockElement `json:"elements,omitempty"`
}

type textObject struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type blockElement struct {
	Type     string     `json:"type"`
	Text     textObject `json:"text"`
	ActionID string     `json:"action_id"`
	Value    string     `json:"value"`
	Style    string     `json:"style,omitempty"`
}

func interactiveMessageBlocks(text string, buttons ...blockElement) []messageBlock {
	blocks := textBlocks(text)
	if len(buttons) > 0 {
		blocks = append(blocks, messageBlock{Type: "actions", Elements: buttons})
	}
	return blocks
}

func textBlocks(text string) []messageBlock {
	runes := []rune(text)
	blocks := make([]messageBlock, 0, (len(runes)+slackSectionTextLimit-1)/slackSectionTextLimit)
	for len(runes) > 0 {
		count := min(len(runes), slackSectionTextLimit)
		blocks = append(blocks, messageBlock{
			Type: "section",
			Text: &textObject{Type: "mrkdwn", Text: string(runes[:count])},
		})
		runes = runes[count:]
	}
	return blocks
}

func button(label, actionID, value, style string) blockElement {
	return blockElement{
		Type:     "button",
		Text:     textObject{Type: "plain_text", Text: label},
		ActionID: actionID,
		Value:    value,
		Style:    style,
	}
}
