package config

import (
	"strings"
	"testing"
)

// Legacy authorization fields from the pre-pairing releases (workspace_id,
// owner_open_id, allowed_open_ids; access_mode never shipped) must load with a
// warning instead of failing: bindings stay dormant until an owner is paired,
// so ignoring the fields never grants anyone access.
func TestLegacyAuthorizationFieldsIgnoredWithWarning(t *testing.T) {
	data := `ims:
  feishu:
    app_id: app
    app_secret: secret
    owner_open_id: ou_legacy
    allowed_open_ids:
      - ou_legacy
      - ou_other
    access_mode: allowlist
    chat_bindings: {chat: project}
  wechat:
    workspace_id: project
workspaces: {project: {path: /project}}
`
	cfg, doc, _, err := ParseDocument([]byte(data))
	if err != nil {
		t.Fatalf("legacy fields must not fail parsing: %v", err)
	}
	issues := strings.Join(UnsupportedConfigFields(doc), "\n")
	for _, field := range []string{"owner_open_id", "allowed_open_ids", "access_mode", "workspace_id"} {
		if !strings.Contains(issues, field+" is not supported") {
			t.Fatalf("legacy field %s not reported: %v", field, issues)
		}
	}
	im := cfg.IMConfigByType("feishu")
	if im == nil || im.OwnerIdentity() != nil {
		t.Fatalf("legacy fields must not synthesize an owner, got %+v", im)
	}
	if im.Bindings()["chat"] != "project" {
		t.Fatalf("chat_bindings must survive, got %v", im.Bindings())
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("bindings without an owner must pass validation (dormant until pairing): %v", err)
	}
}

func TestValidateStillRejectsUnknownBindingWorkspace(t *testing.T) {
	data := "ims: {feishu: {app_id: app, app_secret: secret, chat_bindings: {chat: missing}}}\nworkspaces: {project: {path: /project}}\n"
	cfg, _, _, err := ParseDocument([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || err.Error() != "invalid chat binding" {
		t.Fatalf("dangling binding must still fail validation: %v", err)
	}
}
