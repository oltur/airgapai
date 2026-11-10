package approval

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// CreateTicket files a maintenance ticket. Every field is checked, and no other field is allowed.
var CreateTicket = Tool{
	Name: "create_ticket",
	Validate: func(args map[string]any) error {
		for k := range args {
			switch k {
			case "title", "body", "priority":
			default:
				return fmt.Errorf("unexpected field %q", k)
			}
		}
		title, ok := args["title"].(string)
		if !ok || strings.TrimSpace(title) == "" || utf8.RuneCountInString(title) > 120 || strings.ContainsAny(title, "\r\n") {
			return errors.New("title must be one line of 1 to 120 characters")
		}
		body, ok := args["body"].(string)
		if !ok || utf8.RuneCountInString(body) > 4000 {
			return errors.New("body must be text of at most 4000 characters")
		}
		switch args["priority"] {
		case "low", "normal", "high":
		default:
			return errors.New(`priority must be "low", "normal" or "high"`)
		}
		return nil
	},
}
