---
name: get-started
description: Use right after Kenaz is installed, or when the user asks whether their Kameas Fleet connection works or what Kenaz can do for them. Checks the connection with fleet_status and explains what is available.
---

# Get started with Kenaz

Kenaz connects this conversation to the user's organization's Kameas Fleet through the hosted Kameas Fleet connector.

1. Call `fleet_status`. It takes no arguments and changes nothing.
2. Tell the user, in plain words, what it reports:
   - which Kameas Fleet organization they are connected to;
   - whether Kameas Fleet is available to this app under their organization's plan and settings;
   - whether memory sync is on for their account. Whether this connection may use memory is shown in the Kameas Fleet dashboard; point the user there if they ask.
3. If the connection is not available, show the message from `fleet_status` as it is and suggest the user ask their organization's Kameas Fleet administrator. Don't retry.
4. Otherwise, offer two or three things to try, for example:
   - "Search my team's shared context for our deployment checklist."
   - "Find a shared skill for writing release notes."
   - "What did I save to my memory recently?"

Ground rules:

- Never ask for a password, token or other secret. Signing in happens in Kameas Fleet's own sign-in page, not in this conversation.
- Don't promise features the result of `fleet_status` says are unavailable.
