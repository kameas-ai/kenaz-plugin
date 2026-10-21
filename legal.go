// Package kenaz holds repository-level assets that ship inside the
// binary: license texts and the legally approved public statements.
package kenaz

import _ "embed"

// LicenseApache is this repository's license (Apache-2.0). Official
// binaries additionally ship LICENSE (Kameas Client Software License) and
// THIRD_PARTY_NOTICES, added by the official release pipeline.
//
//go:embed LICENSE
var LicenseApache string

// ThirdPartyNotices are the license texts of every linked dependency
// (scripts/third-party-notices.sh; `make check-notices` keeps it current).
//
//go:embed THIRD_PARTY_NOTICES
var ThirdPartyNotices string

// Notice is this repository's Apache NOTICE.
//
//go:embed NOTICE
var Notice string

// Disclaimer is the third-party trademark disclaimer. It must appear
// verbatim in README.md, at the end of plugin.json's description, and in
// the marketplace entry's description.
const Disclaimer = "Claude and Claude Code are trademarks of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz for Claude Code and Codex is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic or OpenAI."

// OpenCoreStatement is the approved open-source wording.
// Never claim "fully open source". Usable only while this repository is
// public and the release conditions for the claim hold.
const OpenCoreStatement = "What Kenaz for Claude Code and Codex reports about your sessions is decided by open-source code you can read. The part that connects to Kameas Fleet (sign-in and transport) is proprietary."

// OfficialBuildsStatement goes in the README and in the release notes.
const OfficialBuildsStatement = "Release binaries are official builds that include a proprietary module and are licensed under the Kameas Client Software License. The source in this repository is Apache-2.0 and can be built without that module, but such builds cannot connect to Kameas Fleet."

// NoticeFilteringSentence is the public-repository sentence of the Agent Telemetry
// Notice's "Never sent" paragraph. It names this repository, so
// it is valid only while the repository is public. Introducing it was a
// wording change only: no new notice_version and no re-consent.
const NoticeFilteringSentence = "This filtering happens on your device, in code you can read at github.com/kameas-ai/kenaz-plugin, and Fleet rejects anything outside this list again on arrival."
