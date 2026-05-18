# Cookbook

This page collects copyable recipes for installing, configuring, and running `chord-gateway` in common real deployments.

- Chinese version: [cookbook_CN.md](./cookbook_CN.md)

## Choose an installation method

### Option A: install from GitHub Releases

Use this when you want a prebuilt binary and do not need to build from source.

1. Pick a release tag and target from the [GitHub Releases](https://github.com/keakon/chord-gateway/releases) page.

   Current release assets are named like:

   ```text
   chord-gateway-${VERSION}-darwin-amd64.tar.gz
   chord-gateway-${VERSION}-darwin-arm64.tar.gz
   chord-gateway-${VERSION}-linux-amd64.tar.gz
   chord-gateway-${VERSION}-linux-arm64.tar.gz
   chord-gateway-${VERSION}-windows-amd64.zip
   SHA256SUMS
   ```

2. Download and install one asset. Example for Linux amd64:

   ```bash
   VERSION=v0.3.1  # replace with the release you want
   OS=linux
   ARCH=amd64
   ASSET="chord-gateway-${VERSION}-${OS}-${ARCH}.tar.gz"

   curl -LO "https://github.com/keakon/chord-gateway/releases/download/${VERSION}/${ASSET}"
   curl -LO "https://github.com/keakon/chord-gateway/releases/download/${VERSION}/SHA256SUMS"
   grep "  ${ASSET}$" SHA256SUMS | sha256sum -c -

   tar -xzf "${ASSET}"
   sudo install -m 0755 "chord-gateway-${VERSION}-${OS}-${ARCH}/chord-gateway" /usr/local/bin/chord-gateway
   chord-gateway --version
   ```

   If you run the command on macOS, use `shasum -a 256 -c -` instead of `sha256sum -c -`. Replace `VERSION`, `OS`, and `ARCH` for your platform. On Windows, download the `.zip` asset and put `chord-gateway.exe` somewhere on `PATH`.

### Option B: install with Go

Use this when you already have the target Go toolchain locally:

```bash
go install github.com/keakon/chord-gateway@latest
```

The binary is usually installed to `$(go env GOPATH)/bin`. Make sure that directory is on `PATH`, or call the binary by absolute path.

### Option C: build from a checkout

```bash
git clone https://github.com/keakon/chord-gateway.git
cd chord-gateway
go build -o chord-gateway .
./chord-gateway --version
```

## Prepare config and state directories

A predictable layout makes service files and troubleshooting easier:

```bash
mkdir -p ~/.config/chord-gateway ~/.local/state/chord-gateway
# If you are in a source checkout, you can start from the example file:
# cp config.example.yaml ~/.config/chord-gateway/config.yaml
# Otherwise create config.yaml from one of the recipes below.
touch ~/.config/chord-gateway/config.yaml
chmod 700 ~/.config/chord-gateway ~/.local/state/chord-gateway
chmod 600 ~/.config/chord-gateway/config.yaml
```

Edit `~/.config/chord-gateway/config.yaml` before starting the gateway.

Important notes:

- Set `chord_path` to an absolute path if you run under systemd or launchd. Services do not load shell aliases and may have a minimal `PATH`.
- Keep Feishu `app_secret`, WeChat token files, and the whole state directory out of version control.
- Gateway state defaults to `${XDG_STATE_HOME:-~/.local/state}/chord-gateway`.

## Recipe: WeChat only, one workspace

Use this when a single WeChat login should control one workspace.

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
workspaces:
  default:
    path: ~/work/project-a
chord_path: /usr/local/bin/chord
idle_timeout: 30m
```

Run it:

```bash
chord-gateway -f ~/.config/chord-gateway/config.yaml
```

On first run, scan the QR code URL printed by the gateway. The reusable WeChat token is stored under `<state_dir>/wechat/token.json` unless `ims.wechat.token_path` is set.

## Recipe: Feishu only, one workspace

Use this when any allowed Feishu chat should route to one workspace.

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
workspaces:
  default:
    path: ~/work/project-a
chord_path: /usr/local/bin/chord
idle_timeout: 30m
```

Feishu app setup checklist:

1. In the Feishu developer console, use long connection event delivery.
2. Subscribe to `im.message.receive_v1`.
3. Add `card.action.trigger` in callback configuration for interactive cards.
4. Publish a new app version after changing capabilities, permissions, or event subscriptions.
5. Add the bot to the target chat.
6. Send a text message (`text` or `post`) and then `/status`.

To discover `owner_open_id`, temporarily omit it, send one message, and read `open_id=ou_xxx` from the `feishu: received message` log line. Add the value back to config and restart the gateway.

## Recipe: Feishu multiple groups, multiple workspaces

Use this when different Feishu groups should control different repositories.

Recommended rollout:

1. Start with the one-workspace Feishu config above.
2. Create each target Feishu group and add the bot.
3. Send a text message (`text` or `post`) in the group.
4. In that same chat, run `/bind <workspace_id> <path>`.
5. Re-open `config.yaml` and verify that `ims.feishu.chat_bindings` and `workspaces` were written as expected.

Example final config:

```yaml
ims:
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
chord_path: /usr/local/bin/chord
idle_timeout: 30m
```

`/bind` only updates Feishu chat bindings and workspace definitions. Manual edits to other config fields still require a gateway restart.

## Recipe: WeChat plus Feishu

Use this when WeChat should stay on one fixed workspace while Feishu groups route independently.

```yaml
ims:
  wechat:
    base_url: https://ilinkai.weixin.qq.com
    workspace_id: project-a
  feishu:
    app_id: cli_xxx
    app_secret: your-app-secret
    owner_open_id: ou_owner_xxx
    chat_bindings:
      oc_project_a: project-a
      oc_project_b: project-b
workspaces:
  project-a:
    path: ~/work/project-a
  project-b:
    path: ~/work/project-b
chord_path: /usr/local/bin/chord
idle_timeout: 30m
```

Routing behavior:

- All WeChat messages go to `project-a`.
- Feishu chat `oc_project_a` goes to `project-a`.
- Feishu chat `oc_project_b` goes to `project-b`.

## Run as a systemd user service

Use this on Linux when you want the gateway to restart automatically after failures.

1. Make sure config and state directories exist:

   ```bash
   mkdir -p ~/.config/chord-gateway ~/.local/state/chord-gateway ~/.config/systemd/user
   ```

2. Create `~/.config/systemd/user/chord-gateway.service`:

   ```ini
   [Unit]
   Description=chord-gateway
   After=default.target

   [Service]
   Type=simple
   ExecStart=/usr/local/bin/chord-gateway -f %h/.config/chord-gateway/config.yaml
   Restart=on-failure
   RestartSec=5s
   Environment=CHORD_GATEWAY_STATE_DIR=%h/.local/state/chord-gateway

   [Install]
   WantedBy=default.target
   ```

   If you installed with `go install`, change `ExecStart` to `%h/go/bin/chord-gateway ...` or another absolute path.

3. Enable and start it:

   ```bash
   systemctl --user daemon-reload
   systemctl --user enable --now chord-gateway
   journalctl --user -u chord-gateway -f
   ```

4. To keep the user service running after logout, enable lingering if your system requires it:

   ```bash
   sudo loginctl enable-linger "$USER"
   ```

Gateway logs are still written to `<state_dir>/gateway.log`; `journalctl` is useful for service lifecycle logs.

## Run with launchd on macOS

Use this when you want the gateway to start at login.

1. Prepare directories:

   ```bash
   mkdir -p ~/.config/chord-gateway ~/.local/state/chord-gateway ~/Library/LaunchAgents
   ```

2. Create `~/Library/LaunchAgents/io.github.keakon.chord-gateway.plist`.

   Replace `/Users/YOUR_USER` and binary paths with absolute paths. `launchd` does not expand `~` in plist strings.

   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key>
     <string>io.github.keakon.chord-gateway</string>

     <key>ProgramArguments</key>
     <array>
       <string>/usr/local/bin/chord-gateway</string>
       <string>-f</string>
       <string>/Users/YOUR_USER/.config/chord-gateway/config.yaml</string>
     </array>

     <key>EnvironmentVariables</key>
     <dict>
       <key>CHORD_GATEWAY_STATE_DIR</key>
       <string>/Users/YOUR_USER/.local/state/chord-gateway</string>
     </dict>

     <key>RunAtLoad</key>
     <true/>
     <key>KeepAlive</key>
     <true/>

     <key>StandardOutPath</key>
     <string>/Users/YOUR_USER/.local/state/chord-gateway/launchd.out.log</string>
     <key>StandardErrorPath</key>
     <string>/Users/YOUR_USER/.local/state/chord-gateway/launchd.err.log</string>
   </dict>
   </plist>
   ```

3. Load it:

   ```bash
   launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/io.github.keakon.chord-gateway.plist
   launchctl enable "gui/$(id -u)/io.github.keakon.chord-gateway"
   launchctl kickstart -k "gui/$(id -u)/io.github.keakon.chord-gateway"
   ```

4. Inspect logs:

   ```bash
   tail -f ~/.local/state/chord-gateway/gateway.log
   tail -f ~/.local/state/chord-gateway/launchd.err.log
   ```

## Operational checklist before unattended use

Before you leave the gateway running unattended:

- Run `/status` from every IM chat you expect to use.
- Confirm `chord_path` resolves to the intended Chord binary under the service environment.
- Confirm the gateway log path and state directory are writable by the service user.
- For Feishu, configure `owner_open_id` and/or `allowed_open_ids` before exposing the bot beyond local testing.
- Keep backups of `config.yaml` and any state directory that contains WeChat token files if you want login state to survive host replacement.
- Know how to stop the service:
  - systemd user service: `systemctl --user stop chord-gateway`
  - launchd: `launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/io.github.keakon.chord-gateway.plist`
