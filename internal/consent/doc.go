// Package consent gates uploads on sign-in, opt-ins, pending approvals, org pause and lockdown, and renders the Agent Telemetry Notice.
//
// Gate is the hook the telemetry pipeline calls before
// spooling or forwarding a record (Decide), and feeds Fleet errors back
// through ObserveUploadError. Lanes is what the daemon implements so
// withdrawal can stop and purge.
package consent
