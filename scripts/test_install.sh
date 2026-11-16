#!/usr/bin/env bash
# Regression test for scripts/install.sh.
#
# The test drives the installer against a local fixture served over file:// so
# it never touches the network: it builds a fake release archive plus its
# SHA256SUMS, then checks the success path and every failure path that must
# stop the install.
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)
installer="$repo_root/scripts/install.sh"
fixture=$(mktemp -d "${TMPDIR:-/tmp}/chord-gateway-install-test.XXXXXX")
trap 'chmod -R u+rwX "$fixture"; rm -rf "$fixture"' EXIT

fail() {
	echo "install script test failed: $*" >&2
	exit 1
}

bash_path="$BASH"

# The installer warns when chord is not on PATH, so the test hides any chord
# binary from the run to make that warning deterministic.
clean_path=""
IFS=':' read -r -a path_entries <<<"$PATH"
for entry in "${path_entries[@]}"; do
	[ -n "$entry" ] || continue
	[ -x "${entry}/chord" ] && continue
	clean_path="${clean_path:+${clean_path}:}${entry}"
done
[ -n "$clean_path" ] || fail "cannot build a PATH without chord"

tag="v0.3.1"
case "$(uname -s)" in
	Linux) os="linux" ;;
	Darwin) os="darwin" ;;
	*) fail "unsupported test host" ;;
esac
case "$(uname -m)" in
	x86_64 | amd64) arch="amd64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*) fail "unsupported test architecture" ;;
esac
base_url="file://${fixture}/releases/download"
asset="chord-gateway-${tag}-${os}-${arch}.tar.gz"
release_dir="${fixture}/releases/download/${tag}"

bash -n "$installer" || fail "installer does not parse"

mkdir -p "${release_dir}" "${fixture}/stage/chord-gateway-${tag}-${os}-${arch}" "${fixture}/releases/tag"

cat > "${fixture}/stage/chord-gateway-${tag}-${os}-${arch}/chord-gateway" <<'SH'
#!/usr/bin/env bash
echo "chord-gateway test-installer 0.3.1"
SH
chmod +x "${fixture}/stage/chord-gateway-${tag}-${os}-${arch}/chord-gateway"
echo "license text" > "${fixture}/stage/chord-gateway-${tag}-${os}-${arch}/LICENSE"
tar -C "${fixture}/stage" -czf "${release_dir}/${asset}" "chord-gateway-${tag}-${os}-${arch}"

if command -v sha256sum >/dev/null 2>&1; then
	(cd "$release_dir" && sha256sum "$asset" > SHA256SUMS)
else
	(cd "$release_dir" && shasum -a 256 "$asset" > SHA256SUMS)
fi

# The installer resolves the latest release by following the redirect and
# reading the tag from the last path segment, so the fixture only needs a file
# at that path.
: > "${fixture}/releases/tag/${tag}"

install_with() {
	local dir="$1"
	shift
	env \
		PATH="${mock_bin:+${mock_bin}:}${clean_path}" \
		CHORD_GATEWAY_INSTALL_BASE_URL="$base_url" \
		CHORD_GATEWAY_INSTALL_LATEST_URL="file://${fixture}/releases/tag/${tag}" \
		CHORD_GATEWAY_INSTALL_VERSION="" \
		CHORD_GATEWAY_INSTALL_OS="${os_override:-}" \
		CHORD_GATEWAY_INSTALL_ARCH="${arch_override:-}" \
		CHORD_GATEWAY_INSTALL_DIR="$dir" \
		CHORD_GATEWAY_INSTALL_NO_SUDO="${no_sudo:-1}" \
		MOCK_SUDO_MODE="${sudo_mode:-}" \
		MOCK_SUDO_LOG="${fixture}/sudo.log" \
		"$bash_path" "$installer" "$@"
}

expect_install_failure() {
	local dir="$1" output="$2"
	if install_with "$dir" > "$output" 2>&1; then
		fail "installer reported success: $(cat "$output")"
	fi
	if grep -q "Installed chord-gateway" "$output"; then
		fail "installer printed a success message after failing: $(cat "$output")"
	fi
	[ ! -f "${dir}/chord-gateway" ] || fail "a failed install wrote a binary"
}

# 1. The default path resolves the latest release and detects the host target.
install_dir="${fixture}/bin"
if ! install_with "$install_dir" > "${fixture}/latest.log" 2>&1; then
	fail "installer failed: $(cat "${fixture}/latest.log")"
fi
[ -x "${install_dir}/chord-gateway" ] || fail "chord-gateway was not installed"
"${install_dir}/chord-gateway" | grep -q "test-installer" || fail "the installed binary is not the fixture binary"
grep -q "Checksum verified" "${fixture}/latest.log" || fail "the installer did not report checksum verification"
grep -q "chord is not on your PATH" "${fixture}/latest.log" || fail "the installer did not report the missing Chord dependency"

# 2. An explicit version installs into a fresh directory.
pinned_dir="${fixture}/bin-pinned"
if ! install_with "$pinned_dir" --version "$tag" > "${fixture}/pinned.log" 2>&1; then
	fail "installer failed with an explicit version: $(cat "${fixture}/pinned.log")"
fi
[ -x "${pinned_dir}/chord-gateway" ] || fail "chord-gateway was not installed with an explicit version"

# 3. A checksum mismatch stops the install.
bad_release="${fixture}/bad/download/${tag}"
mkdir -p "$bad_release"
cp "${release_dir}/${asset}" "${bad_release}/${asset}"
printf '%s  %s\n' \
	"0000000000000000000000000000000000000000000000000000000000000000" "$asset" \
	> "${bad_release}/SHA256SUMS"
base_url="file://${fixture}/bad/download"
bad_dir="${fixture}/bin-bad"
if install_with "$bad_dir" > "${fixture}/bad.log" 2>&1; then
	fail "the installer accepted an archive with a mismatched checksum"
fi
grep -q "checksum mismatch" "${fixture}/bad.log" || fail "the installer did not report the checksum mismatch"
[ ! -e "${bad_dir}/chord-gateway" ] || fail "the installer installed a binary with a mismatched checksum"
base_url="file://${fixture}/releases/download"

# 4. A missing asset stops the install.
empty_release="${fixture}/empty/download/${tag}"
mkdir -p "$empty_release"
base_url="file://${fixture}/empty/download"
if install_with "${fixture}/bin-empty" > "${fixture}/empty.log" 2>&1; then
	fail "the installer accepted a release without the requested asset"
fi
grep -q "failed to download" "${fixture}/empty.log" || fail "the installer did not report the missing asset"
base_url="file://${fixture}/releases/download"

# 5. An unsupported architecture stops the install.
arch_override="riscv64"
if install_with "${fixture}/bin-arch" > "${fixture}/arch.log" 2>&1; then
	fail "the installer accepted an unsupported architecture"
fi
grep -q "unsupported architecture" "${fixture}/arch.log" || fail "the installer did not report the unsupported architecture"
arch_override=""

# 6. An unwritable install directory without sudo stops the install and points
#    at the alternative flag.
if [ "$(id -u)" -ne 0 ]; then
	readonly_dir="${fixture}/readonly"
	mkdir -p "$readonly_dir"
	chmod 555 "$readonly_dir"
	no_sudo="1"
	if install_with "$readonly_dir" > "${fixture}/readonly.log" 2>&1; then
		fail "the installer wrote to an unwritable directory"
	fi
	grep -q -- "--install-dir" "${fixture}/readonly.log" ||
		fail "the installer did not point at the --install-dir alternative"
	no_sudo="1"
fi

# 7. A release tag that is not a version stops the install.
if install_with "${fixture}/bin-tag" --version "not-a-tag" > "${fixture}/tag.log" 2>&1; then
	fail "the installer accepted an invalid release tag"
fi
grep -q "unexpected release tag" "${fixture}/tag.log" || fail "the installer did not report the invalid release tag"

# 8. --help succeeds and an unknown option fails.
"$bash_path" "$installer" --help > "${fixture}/help.log" 2>&1 || fail "--help failed"
grep -q -- "--install-dir" "${fixture}/help.log" || fail "--help did not print the options"
if "$bash_path" "$installer" --nope > "${fixture}/nope.log" 2>&1; then
	fail "the installer accepted an unknown option"
fi

# 9. A directory at the binary's exact target must not receive a nested copy.
directory_target="${fixture}/directory-target"
mkdir -p "${directory_target}/chord-gateway"
expect_install_failure "$directory_target" "${fixture}/directory-target.log"
[ ! -e "${directory_target}/chord-gateway/chord-gateway" ] ||
	fail "installer copied the binary inside the target directory"

# 10. A failed native install must return an error even inside a Bash condition.
install_mock="${fixture}/install-mock"
mkdir -p "$install_mock"
cat > "${install_mock}/install" <<'SH'
#!/usr/bin/env bash
echo "fixture install failure" >&2
exit 23
SH
chmod +x "${install_mock}/install"
mock_bin="$install_mock"
expect_install_failure "${fixture}/copy-failure" "${fixture}/copy-failure.log"
grep -q "fixture install failure" "${fixture}/copy-failure.log" ||
	fail "the copy-failure fixture did not reach install"
mock_bin=""

# sudo is always a fixture: tests must never prompt or perform real escalation.
sudo_mock="${fixture}/sudo-mock"
mkdir -p "$sudo_mock"
cat > "${sudo_mock}/sudo" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_SUDO_LOG"
case "$MOCK_SUDO_MODE" in
	deny-create)
		[ "$1" != "mkdir" ] || { echo "fixture sudo mkdir failure" >&2; exit 23; }
		;;
	deny-copy)
		[ "$1" != "install" ] || { echo "fixture sudo install failure" >&2; exit 23; }
		;;
	allow-create)
		if [ "$1" = "mkdir" ]; then
			dir="${!#}"
			parent="${dir%/*}"
			chmod u+w "$parent"
			status=0
			"$@" || status=$?
			chmod 500 "$parent"
			exit "$status"
		fi
		;;
esac
if [ "$1" = "install" ] && [ "$2" = "-m" ]; then
	dir="${!#}"
	dir="${dir%/}"
	case "$(uname -s)" in
		Darwin) mode="$(stat -f '%Lp' "$dir")" ;;
		Linux) mode="$(stat -c '%a' "$dir")" ;;
	esac
	# Grant fixture-only write/traverse access for the copy, then restore the mode that
	# the installer left before this call. Earlier chmods remain observable.
	chmod u+wx "$dir"
	status=0
	"$@" || status=$?
	chmod "$mode" "$dir"
	exit "$status"
fi
exec "$@"
SH
chmod +x "${sudo_mock}/sudo"

if [ "$(id -u)" -ne 0 ]; then
	mock_bin="$sudo_mock"
	no_sudo="0"

	# 11. Privileged directory creation failure must stop before the copy.
	denied_parent="${fixture}/denied-parent"
	mkdir -p "$denied_parent"
	chmod 500 "$denied_parent"
	sudo_mode="deny-create"
	: > "${fixture}/sudo.log"
	expect_install_failure "${denied_parent}/bin" "${fixture}/sudo-create.log"
	grep -q "fixture sudo mkdir failure" "${fixture}/sudo-create.log" ||
		fail "the sudo-create fixture did not reach mkdir"
	if grep -q '^install ' "${fixture}/sudo.log"; then
		fail "installer attempted to copy after privileged mkdir failed"
	fi
	chmod 700 "$denied_parent"

	# 12. A newly created privileged install directory remains accessible.
	create_parent="${fixture}/create-parent"
	mkdir -p "$create_parent"
	chmod 500 "$create_parent"
	sudo_mode="allow-create"
	if ! install_with "${create_parent}/bin" > "${fixture}/sudo-success.log" 2>&1; then
		fail "privileged directory creation failed: $(cat "${fixture}/sudo-success.log")"
	fi
	[ -f "${create_parent}/bin/chord-gateway" ] && [ -x "${create_parent}/bin/chord-gateway" ] ||
		fail "privileged directory creation did not install the binary"
	case "$os" in
		darwin) mode="$(stat -f '%Lp' "${create_parent}/bin")" ;;
		linux) mode="$(stat -c '%a' "${create_parent}/bin")" ;;
	esac
	[ "$mode" = "755" ] || fail "new privileged install directory has mode $mode"
	chmod 700 "$create_parent"

	# 13. A denied privileged copy must not turn into a successful install.
	private_dir="${fixture}/private-bin"
	mkdir -p "$private_dir"
	echo "generic fixture data" > "${private_dir}/data"
	chmod 500 "$private_dir"
	sudo_mode="deny-copy"
	expect_install_failure "$private_dir" "${fixture}/sudo-copy.log"
	grep -q "fixture sudo install failure" "${fixture}/sudo-copy.log" ||
		fail "the sudo-copy fixture did not reach install"

	# 14. Installing into an existing directory must preserve its permissions.
	sudo_mode="allow"
	: > "${fixture}/sudo.log"
	if ! install_with "$private_dir" > "${fixture}/private.log" 2>&1; then
		fail "privileged fixture install failed: $(cat "${fixture}/private.log")"
	fi
	[ -f "${private_dir}/chord-gateway" ] && [ -x "${private_dir}/chord-gateway" ] ||
		fail "privileged fixture did not install the binary"
	case "$os" in
		darwin) mode="$(stat -f '%Lp' "$private_dir")" ;;
		linux) mode="$(stat -c '%a' "$private_dir")" ;;
	esac
	[ "$mode" = "500" ] || fail "installer changed existing directory mode to $mode"
	if grep -q '^install -d ' "${fixture}/sudo.log"; then
		fail "installer reset the existing directory with install -d"
	fi
	chmod 700 "$private_dir"

	# 15. An inaccessible target must be checked by the privileged copy itself.
	hidden_dir="${fixture}/hidden-target"
	mkdir -p "${hidden_dir}/chord-gateway"
	chmod 400 "$hidden_dir"
	sudo_mode="allow"
	expect_install_failure "$hidden_dir" "${fixture}/hidden-target.log"
	chmod 700 "$hidden_dir"
	[ ! -e "${hidden_dir}/chord-gateway/chord-gateway" ] ||
		fail "privileged install copied into an inaccessible directory target"
	mock_bin=""
else
	echo "skip permission fixtures: running as root"
fi

echo "install script test passed"
