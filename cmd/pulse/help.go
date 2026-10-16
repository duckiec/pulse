package main

import (
	"fmt"
	"io"
)

// This file is the `pulse help` system: one help text per command plus a
// topic dispatcher so `pulse help`, `pulse --help`, `pulse <cmd> --help`,
// `pulse <cmd> help`, and `pulse help <cmd> [sub]` all agree.
//
// Conventions (kept uniform across every entry):
//   - Explicit help goes to stdout and exits 0; usage errors go to stderr
//     and exit 2 (AGENTS.md §4.4). FlagSet.Usage keeps pointing at the same
//     strings so a parse failure and an explicit request cannot disagree.
//   - The bare token `help` is a help request only when it is the sole
//     trailing token of a leaf command (`pulse vault set help`). A key
//     literally named `help` can still be set with an inline value
//     (`pulse vault set help <value>`).

// isHelpToken reports whether s is an explicit help request.
func isHelpToken(s string) bool {
	return s == "help" || s == "--help" || s == "-h"
}

// wantsHelp reports whether any of args is an explicit help request.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if isHelpToken(a) {
			return true
		}
	}
	return false
}

// wantsFlagHelp reports whether any of args is a help FLAG (-h/--help).
// Bare `help` is excluded: at leaf level it may be a real value (e.g. a
// vault key), so only the flag forms count here.
func wantsFlagHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

func printHelp(stdout io.Writer, text string) {
	fmt.Fprintln(stdout, text)
}

const topHelp = `pulse — offline, Git-native API workbench (TUI + headless MCP server)

usage:
  pulse [flags]
  pulse <file.http>
  pulse mcp
  pulse test <dir> [flags]
  pulse vault <command> [args] [flags]
  pulse init [file] [--force]
  pulse doctor [--key-id ID]
  pulse sync serve <dir> | pulse sync status
  pulse help [command [subcommand]]

commands:
  mcp     serve the MCP server (JSON-RPC over stdio)
  test    run .http collections headless (TAP or JSON)
  vault   seal, list, and remove secrets (.pulse.vault + keychain)
  init    scaffold a workspace (quickstart.http)
  doctor  diagnose workspace, vault, store, and MCP config
  sync    serve P2P sync or inspect its status
  help    show this help or help for a command

flags:
  --version   print version and exit
  --mcp       run in MCP mode (same as ` + "`pulse mcp`" + `)
  -h, --help  show help (also ` + "`pulse help`" + `, ` + "`pulse <command> help`" + `)

examples:
  pulse quickstart.http
  pulse test . --format tap
  pulse vault set API_TOKEN <value>
  pulse doctor
  pulse help vault set

exit codes: 0 ok, 1 failure, 2 usage/parse error, 3 secret-resolution failure, 130 interrupted.
run ` + "`pulse help <command>`" + ` for command details.`

const mcpHelp = `usage: pulse mcp

Serve the headless MCP server: JSON-RPC 2.0 over stdio with the spec
lifecycle initialize -> initialized -> operation loop -> shutdown -> exit.
Stdout is the protocol channel: nothing but JSON-RPC may be written there;
diagnostics go to stderr.

  pulse mcp
  pulse --mcp
  pulse mcp --help

exit codes: 0 clean session end, 1 boot or session failure, 2 stray argument.`

const testHelp = `usage: pulse test <dir> [--parallel N] [--timeout D] [--strict-blocked] [--format tap|json] [--silent]

Run every .http collection under <dir> headless and report TAP 13 (default)
or JSON. Assertions (# @assert) decide pass/fail per request.

  pulse test . --format tap
  pulse test ./collections --parallel 8 --timeout 30s
  pulse test . --format json --silent
  pulse test --help

exit codes: 0 pass, 1 failure, 2 usage/parse error, 3 secret-resolution failure, 130 interrupted.`

const vaultHelp = `usage: pulse vault <command> [args] [flags]

Seal secrets outside .http files. Values resolve at runtime: OS keychain,
then ephemeral in-memory .env, then the decrypted .pulse.vault, then a
hard error (exit 3). Raw values never touch stdout, logs, or the store.

commands:
  init                          create the vault key in the keychain
  keygen                        mint the device identity (prints two public halves)
  encrypt <path.env>            seal a .env file and remove the plaintext
  decrypt <path.env.vault>      restore the plaintext
  add-recipient <pulse-pub-...> append a recipient key
  rotate <path.env>             re-seal for the current recipient list
  set <KEY> [VALUE]             seal one value (prompts or reads stdin without VALUE)
  list                          print sorted key names only, never values
  unset <KEY>                   remove one key

flags (where shown per subcommand):
  --key-id ID          keychain account suffix; empty selects the default
  --out PATH           plaintext output path for decrypt
  --recipients FILE    recipient list for rotate

  pulse vault set API_TOKEN <value>
  pulse vault list
  pulse help vault set`

const vaultInitHelp = `usage: pulse vault init [--key-id ID]

Create the vault key in the OS keychain. Idempotent: re-running names the
same account.

  pulse vault init
  pulse vault init --key-id work`

const vaultKeygenHelp = `usage: pulse vault keygen [--key-id ID]

Mint the device identity and print its two shareable public halves to
stdout (safe to copy). The private halves stay in the keychain.

  pulse vault keygen`

const vaultEncryptHelp = `usage: pulse vault encrypt <path.env> [--key-id ID] [--recipients FILE]

Seal a .env file into <path.env.vault> and remove the plaintext.

  pulse vault encrypt .env`

const vaultDecryptHelp = `usage: pulse vault decrypt <path.env.vault> [--key-id ID] [--out PATH]

Restore the plaintext. Default output is the .env path; --out overrides it.

  pulse vault decrypt .env.vault
  pulse vault decrypt .env.vault --out /tmp/restored.env`

const vaultAddRecipientHelp = `usage: pulse vault add-recipient <pulse-pub-...> [--recipients FILE]

Append a recipient public key to the recipient list.

  pulse vault add-recipient pulse-pub-abc123...`

const vaultRotateHelp = `usage: pulse vault rotate <path.env> [--key-id ID] [--recipients FILE]

Re-seal the vault for the current recipient list.

  pulse vault rotate .env`

const vaultSetHelp = `usage: pulse vault set <KEY> [VALUE]

Seal one value. With VALUE the value is inline; without it the command
prompts securely on a TTY (no echo) or reads piped stdin to EOF. Empty
input fails closed, never a dummy. To operate on a key literally named
` + "`help`" + `, pass its value inline: ` + "`pulse vault set help <value>`" + `.

  pulse vault set API_TOKEN <value>
  pulse vault set API_TOKEN        # prompts, then seals
  echo -n secret | pulse vault set API_TOKEN
  pulse vault set --help`

const vaultListHelp = `usage: pulse vault list

Print sorted key names, one per line, never values.

  pulse vault list`

const vaultUnsetHelp = `usage: pulse vault unset <KEY>

Remove one key from the vault. A key literally named ` + "`help`" + ` is
shown this help text when it is the sole argument; remove it by passing an
unambiguous form (any flag after the key skips the help shortcut).

  pulse vault unset API_TOKEN`

const initHelp = `usage: pulse init [file] [--force]

Scaffold a workspace: writes quickstart.http (default) with a GET, a POST,
an authenticated request ({{DEMO_TOKEN}}), and a @wasm-sign example.
Refuses to overwrite unless --force. To create a file literally named
` + "`help`" + `, write it explicitly: ` + "`pulse init ./help`" + `.

  pulse init
  pulse init api.http --force
  pulse init --help`

const doctorHelp = `usage: pulse doctor [--key-id ID]

Diagnose the workspace: keyring reachability, .pulse.vault integrity,
SQLite health, .http syntax plus the secret cross-check, then the
ready-to-paste mcpServers JSON block.

  pulse doctor
  pulse doctor --key-id work

exit codes: 0 healthy, 2 syntax errors, 3 a secret is set nowhere, 1 infrastructure failure.`

const syncHelp = `usage: pulse sync serve <dir> | pulse sync status

  pulse sync serve <dir>   publish <dir> over P2P until SIGINT or stdin EOF
  pulse sync status        print the last persisted snapshot digest as JSON (never joins)
  pulse help sync serve
  pulse sync --help`

const syncServeHelp = `usage: pulse sync serve <dir>

Publish <dir> over P2P sync: gossip rounds, re-convergence, and snapshot
persistence run until SIGINT or stdin EOF, then the advertisement is
withdrawn and the final snapshot persisted.

  pulse sync serve ./collections`

const syncStatusHelp = `usage: pulse sync status

Print the last persisted sync snapshot digest as JSON. Never joins the
network: no ports, no announce.

  pulse sync status`

// vaultSubHelp prints help for `pulse vault <sub>`; false means unknown sub.
func vaultSubHelp(sub string, stdout, stderr io.Writer) bool {
	var text string
	switch sub {
	case "init":
		text = vaultInitHelp
	case "keygen":
		text = vaultKeygenHelp
	case "encrypt":
		text = vaultEncryptHelp
	case "decrypt":
		text = vaultDecryptHelp
	case "add-recipient":
		text = vaultAddRecipientHelp
	case "rotate":
		text = vaultRotateHelp
	case "set":
		text = vaultSetHelp
	case "list":
		text = vaultListHelp
	case "unset":
		text = vaultUnsetHelp
	default:
		return false
	}
	printHelp(stdout, text)
	return true
}

// runHelp implements `pulse help [command [subcommand]]`.
func runHelp(args []string, stdout, stderr io.Writer) int {
	// `pulse help --help` is still a help request, never an unknown topic.
	if len(args) == 0 || isHelpToken(args[0]) {
		printHelp(stdout, topHelp)
		return exitOK
	}
	switch args[0] {
	case "mcp":
		printHelp(stdout, mcpHelp)
		return exitOK
	case "test":
		printHelp(stdout, testHelp)
		return exitOK
	case "vault":
		if len(args) > 1 {
			// `pulse help vault --help` is the vault help, not an
			// unknown subcommand.
			if isHelpToken(args[1]) {
				printHelp(stdout, vaultHelp)
				return exitOK
			}
			if vaultSubHelp(args[1], stdout, stderr) {
				return exitOK
			}
			fmt.Fprintf(stderr, "pulse help: unknown vault subcommand %q\n", args[1])
			return exitUsage
		}
		printHelp(stdout, vaultHelp)
		return exitOK
	case "init":
		printHelp(stdout, initHelp)
		return exitOK
	case "doctor":
		printHelp(stdout, doctorHelp)
		return exitOK
	case "sync":
		// `pulse help sync --help` is the sync help, not an unknown subcommand.
		if len(args) > 1 && isHelpToken(args[1]) {
			printHelp(stdout, syncHelp)
			return exitOK
		}
		if len(args) > 1 {
			switch args[1] {
			case "serve":
				printHelp(stdout, syncServeHelp)
				return exitOK
			case "status":
				printHelp(stdout, syncStatusHelp)
				return exitOK
			default:
				fmt.Fprintf(stderr, "pulse help: unknown sync subcommand %q\n", args[1])
				return exitUsage
			}
		}
		printHelp(stdout, syncHelp)
		return exitOK
	case "help":
		printHelp(stdout, topHelp)
		return exitOK
	default:
		fmt.Fprintf(stderr, "pulse help: unknown topic %q\n", args[0])
		return exitUsage
	}
}
