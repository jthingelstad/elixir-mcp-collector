package v2

// releasePublicKeys is the key a release must be signed with before
// self-update installs it (trust.go, issue #5): one OpenSSH ed25519
// public key per line, exactly as `ssh-keygen` writes a .pub file. A
// second line is how the key rotates - a release that trusts both is
// signed with the old one, and every release after it with the new.
//
// It is compiled in, and only the maintainer's GitHub Actions secret
// COLLECTOR_SIGNING_KEY holds the private half, so neither the hub nor
// a release page can hand a collector code the key did not sign.
//
// Until the maintainer generates the keypair this is a placeholder that
// parses as no key at all: release.yml refuses to build
// (TestCompiledReleaseKey), and a build carrying it installs nothing.
// SECURITY.md has the commands. README.md, SECURITY.md and
// docs/recipes/cloud-init.yaml publish the same line for operators to
// verify an installer with; TestPublishedKeyMatchesTheCompiledOne keeps
// them in step.
var releasePublicKeys = "ssh-ed25519 REPLACE_WITH_THE_RELEASE_PUBLIC_KEY elixir-mcp-collector-release"
