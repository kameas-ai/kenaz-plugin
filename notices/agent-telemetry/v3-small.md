**Share agent usage with {Organization name}?**

Kenaz can send {Organization name}'s Fleet a summary of how you use {harnesses} on this device. Nothing is shared until you choose below, and you can change your choice at any time with `kenaz optin`.

**What can be sent, by class** (classes your plan doesn't include are shown greyed out):

- **Usage counts:** when a session starts and ends, how long it lasts, how many tokens it used and its estimated cost.
- **Tool calls:** which kind of tool was used (for example Bash, Edit or Read, or simply "MCP" or "other" for tools you or your organization added), whether it worked and how long it took.
- **Errors:** the kind of error (for example "rate limited" or "network") and whether it was recoverable. Never the error message.
- **Model calls:** for each request to an AI model, the model's name, the provider, token counts, estimated cost, timing, why the response ended, and a coded session reference that groups calls from one session without revealing the session itself.

**Attached to every record:** your Kameas account, your organization, this device's ID and display name ("{device display name}"), and which tool sent it ({harness versions}).

**Never sent:** your prompts, AI responses, tool inputs or outputs, file contents, source code, file paths, folder or repository names, error messages, or the names of tools and servers you added. This filtering happens on your device, in code you can read at github.com/kameas-ai/kenaz-plugin, and Fleet rejects anything outside this list again on arrival.

**Who sees it:** {Organization name}'s Fleet administrators see team-level totals, for example sessions, cost and tool use per day. They do not see your individual sessions in analytics. Because your organization is small, administrators see only organization-wide totals. They can see that this device is connected and when it last checked in. Your organization has agreed not to use this data to evaluate individual members. Kameas processes it on your organization's behalf.

**How long it is kept:** for as long as {Organization name} uses Kameas Fleet, unless you opt out or leave. Opting out of a class deletes your stored records of that class within 72 hours.

**Your rights:** your organization is responsible for this data. Contact your organization's administrator to access, export or delete it. Kameas's Privacy Policy, section 13, explains more.

Separately, Kenaz may sync skills and team context you choose to install, publish or add. What you publish is shared with your organization as you would expect. Kenaz never changes or blocks what your AI tools do.
