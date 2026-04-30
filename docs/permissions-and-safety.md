# Permissions & Safety

`chord-gateway` is a remote control plane for a local `chord headless` process. Anyone who can send accepted IM messages to the gateway may influence what Chord does in the configured workspace.

## Security boundary

The gateway is not a multi-tenant sandbox.

Treat an allowed IM sender as someone who can interact with Chord in the configured workspace. Depending on Chord configuration and permissions, that may include reading files, editing files, running tools, or requesting model actions.

## Recommended deployment practices

- Use a dedicated machine, user account, container, or VM for gateway deployments when possible.
- Point `workspaces.<id>.path` only at projects that should be controllable from IM.
- Do not point a workspace at your whole home directory.
- Keep Chord permissions and tool approvals conservative.
- Monitor logs for unexpected senders, chats, commands, or workspace routing.

## Feishu access control

For Feishu, configure `owner_open_id` and/or `allowed_open_ids` when deploying beyond local testing.

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
    allowed_open_ids:
      - ou_teammate_xxx
```

Behavior:

- If neither field is set, all users are allowed.
- If either field is set, only listed `open_id`s are allowed.
- `owner_open_id` is included in the effective allowlist.
- Rejected messages are silently ignored.

## Credential handling

Treat these as sensitive:

- Feishu `app_secret`
- WeChat token files (`<state_dir>/wechat/token.json` by default, or `ims.wechat.token_path`)
- Gateway state directory
- Chord provider credentials and auth files

Do not commit secrets or state directories to version control.

## Multi-workspace safety

For Feishu multi-workspace mode, configure `ims.feishu.chat_bindings` so each Feishu chat ID maps to the intended workspace.

WeChat supports only one workspace. Use Feishu if you need separate chat-to-workspace bindings.

## Incident response

If you suspect unauthorized access:

1. Stop the gateway.
2. Revoke or rotate Feishu app secrets.
3. Remove or rotate WeChat token files (`<state_dir>/wechat/token.json` by default, or `ims.wechat.token_path`).
4. Review gateway logs and Chord session history.
5. Review changes in configured workspaces.
6. Restart with a stricter allowlist and workspace scope.

To report vulnerabilities in the project, follow [SECURITY.md](../SECURITY.md).
