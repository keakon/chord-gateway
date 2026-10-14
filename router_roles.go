package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Card values are exact names; numeric text choices index the live role list.
func selectRoleName(target string, roles []RoleInfo, fromCard bool, hints ...func(string) string) (string, error) {
	name := strings.TrimSpace(target)
	if n, err := strconv.Atoi(name); err == nil && !fromCard {
		if n < 1 || n > len(roles) {
			return "", fmt.Errorf("invalid role selection. Send %s to see the current roles", commandHint("/role", hints))
		}
		name = strings.TrimSpace(roles[n-1].Name)
	}
	if !roleNameAvailable(name, roles) {
		return "", fmt.Errorf("%q is not an available role. Send %s to see the available roles", name, commandHint("/role", hints))
	}
	return name, nil
}

func interactionRequestID(msg IncomingMessage) string {
	if msg.InternalAction != nil {
		return msg.InternalAction.RequestID
	}
	return ""
}
