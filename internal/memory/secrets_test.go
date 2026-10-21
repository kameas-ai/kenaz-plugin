package memory

import (
	"strings"
	"testing"
)

// Credential-shaped strings are assembled at run time so no literal in
// this file looks like a credential to repository scanners.
func TestLooksLikeSecret(t *testing.T) {
	r := strings.Repeat
	secret := map[string]string{
		"access key id":     "id=" + "AK" + "IA" + r("Z", 16),
		"temp access key":   "AS" + "IA" + r("7", 16),
		"github personal":   "gh" + "p_" + r("a", 36),
		"github oauth":      "gh" + "o_" + r("b", 36),
		"github server":     "token " + "gh" + "s_" + r("c", 36),
		"github user":       "gh" + "u_" + r("d", 36),
		"github refresh":    "gh" + "r_" + r("e", 36),
		"github fine":       "git" + "hub_pat_" + r("f", 22) + "_" + r("g", 59),
		"pem key":           "-----BEGIN " + "PRIVATE KEY-----\nMII",
		"pem rsa key":       "-----BEGIN RSA " + "PRIVATE KEY-----",
		"pem openssh key":   "-----BEGIN OPENSSH " + "PRIVATE KEY-----",
		"chat bot token":    "xo" + "xb-" + r("1", 12) + "-" + r("a", 24),
		"chat user token":   "xo" + "xp-" + r("2", 12),
		"payments live key": "s" + "k_live_" + r("h", 24),
		"payments rk":       "r" + "k_live_" + r("i", 24),
		"provider key":      "s" + "k-ant-api03-" + r("j", 40),
		"generic sk key":    "s" + "k-proj-" + r("k", 40),
		"jwt":               "Bearer " + "ey" + "J" + r("h", 20) + "." + "eyJ" + r("p", 30) + "." + r("s", 43),
	}
	for name, s := range secret {
		if !LooksLikeSecret(s) {
			t.Errorf("%s: not caught", name)
		}
	}
	benign := []string{
		"Run make check before pushing.",
		"AKIA is the prefix of an access key id",
		"ghp_ tokens are personal access tokens",
		"use sk-learn for that",                 // short
		"the task-runner sk-short-name is fine", // under 20 chars after sk-
		"-----BEGIN PUBLIC KEY-----",
		"eyJ alone is not a token",
		"skill_live_reload",
	}
	for _, s := range benign {
		if LooksLikeSecret(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}
