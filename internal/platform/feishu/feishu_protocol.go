// Package main: Feishu wire-format helpers shared between handlers.
package feishu

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// parseFeishuMessageText extracts visible text from a Feishu message payload.
// Supports "text" and "post" message types; other types collapse to "".
func parseFeishuMessageText(messageType, contentRaw string) (string, error) {
	var content FeishuMessageContent
	if err := json.Unmarshal([]byte(contentRaw), &content); err != nil {
		return "", err
	}
	switch messageType {
	case "text":
		return content.Text, nil
	case "post":
		var lines []string
		for _, line := range content.Content {
			var b strings.Builder
			for _, item := range line {
				switch item.Tag {
				case "text", "a":
					b.WriteString(item.Text)
				case "at":
					if item.UserName != "" {
						b.WriteString("@")
						b.WriteString(item.UserName)
					}
				}
			}
			lines = append(lines, b.String())
		}
		return strings.TrimSpace(strings.Join(lines, "\n")), nil
	default:
		return "", nil
	}
}

// IsValidFeishuCardAction reports whether a card action callback carries a
// known (action_type, action, value) triple. formAnswers carries the option
// numbers of a multi-select form submission, which is a valid answer even
// though the submit button itself binds no option value. Unknown combinations
// are rejected before being enqueued.
func IsValidFeishuCardAction(actionType, action, value string, formAnswers []string) bool {
	switch actionType {
	case "confirm":
		return action == "allow" || action == "deny"
	case "question":
		return action == "answer" && (strings.TrimSpace(value) != "" || len(formAnswers) > 0)
	case "role":
		return action == "switch" && strings.TrimSpace(value) != ""
	default:
		return false
	}
}

// Multi-select question cards identify form fields by name; the card builder
// and the callback parser share these so the two sides cannot drift.
const (
	FeishuQuestionFormName     = "question_form"
	FeishuQuestionSubmitName   = "question_submit"
	feishuQuestionOptionPrefix = "option_"
)

// FeishuQuestionOptionName is the form field name of the checker for a 1-based
// option number.
func FeishuQuestionOptionName(number int) string {
	return feishuQuestionOptionPrefix + strconv.Itoa(number)
}

// feishuQuestionOptionNumber parses a form field name back into its 1-based
// option number.
func feishuQuestionOptionNumber(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, feishuQuestionOptionPrefix)
	if !ok {
		return 0, false
	}
	number, err := strconv.Atoi(rest)
	if err != nil || number < 1 {
		return 0, false
	}
	return number, true
}

// feishuQuestionFormAnswers reads checker values from form_value[name].
// Only booleans represent a selection; unknown shapes must not invent answers.
func feishuQuestionFormAnswers(formValue map[string]interface{}) ([]string, error) {
	numbers := make([]int, 0, len(formValue))
	for name, value := range formValue {
		number, ok := feishuQuestionOptionNumber(name)
		if !ok {
			continue
		}
		checked, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("invalid checker value for %s", name)
		}
		if checked {
			numbers = append(numbers, number)
		}
	}
	sort.Ints(numbers)
	answers := make([]string, 0, len(numbers))
	for _, number := range numbers {
		answers = append(answers, strconv.Itoa(number))
	}
	return answers, nil
}

// derefString returns the pointed-to value, or "" if nil. Used for the many
// optional string fields on the Lark SDK event types.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
