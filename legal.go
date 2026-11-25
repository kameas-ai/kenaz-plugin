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
// verbatim in README.md and plugin/README.md and at the end of
// plugin/.codex-plugin/plugin.json's description (read by Codex).
const Disclaimer = "Claude and Claude Code are trademarks of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz for Claude Code and Codex is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic or OpenAI."

// DisclaimerGAnthropic is the trademark notice for surfaces that name only
// Anthropic products. It ends the Claude plugin manifest's description
// (Claude Code and Cowork).
const DisclaimerGAnthropic = "Claude, Claude Code and Cowork are trademarks of Anthropic, PBC. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic."

// DisclaimerGAnthropicOpenAI is the trademark notice for surfaces that name
// both Anthropic and OpenAI products. It ends the marketplace descriptions.
const DisclaimerGAnthropicOpenAI = "Claude, Claude Code and Cowork are trademarks of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic or OpenAI."

// ClaudePluginDescription is the approved description in
// plugin/.claude-plugin/plugin.json, read by Claude Code and Cowork and
// shown in the Claude directory.
const ClaudePluginDescription = "Kenaz, a Kameas AI product. Connects Claude Code and Cowork on your computer to your organization's Kameas Fleet: shared skills, team context and your private memory, and in Claude Code, usage summaries you choose to share. Requires the kenaz program and a Kameas Fleet account. " + DisclaimerGAnthropic

// MarketplaceDescription is the approved description of the kenaz entry in
// .claude-plugin/marketplace.json, imported by Claude Code and Codex.
const MarketplaceDescription = "Kenaz, a Kameas AI product. Connects AI tools such as Claude Code, Cowork and Codex to your organization's Kameas Fleet: shared skills, team context, your private memory and, where you choose, usage summaries. Requires the kenaz program and a Kameas Fleet account. " + DisclaimerGAnthropicOpenAI

// MarketplaceMetadataDescription is the marketplace's own description.
const MarketplaceMetadataDescription = "Kenaz plugins. " + DisclaimerGAnthropicOpenAI

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
