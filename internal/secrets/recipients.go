package secrets

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
)

// The `pulse.recipients` file (ROADMAP.md §4.3.2/§4.3.4): a Git-trackable
// plaintext list of authorized X25519 decryption public keys, one per line.
//
//   - blank lines and `#` comments are ignored, so the file documents itself;
//   - every other line must be a `pulse-pub-…` shareable key (DecodePub), so
//     a signing key or a typo fails at parse time with the role named;
//   - duplicates fail closed: a list that names Alice twice is either a
//     merge artifact or an editing mistake, and either way re-sealing it
//     would mint two slots for one recipient — refused, not deduped.
//
// The file holds PUBLIC keys only. It is committable by design; the G-secret
// gate must never flag it, and no private half may ever appear in it.
//
// Rotation and revocation need no code here: rotation is re-Seal2 over a new
// list with a fresh DEK, revocation is removing a key before re-sealing
// (ADL-0031). A separate rotate path would be a second seal implementation
// to drift from the first.

// DefaultRecipientsFile is the conventional recipient list name.
const DefaultRecipientsFile = "pulse.recipients"

// ParseRecipients reads a recipients file into raw 32-byte public keys.
//
// Order is preserved (file order = slot order), because slot order is part
// of the sealed bytes and a consumer diffing two seals needs stability.
func ParseRecipients(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: open %s: %w", path, err)
	}
	defer f.Close()

	var out [][]byte
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		raw, err := DecodePub(IdentityPubPrefix, text)
		if err != nil {
			return nil, fmt.Errorf("secrets: %s line %d: %w", path, line, err)
		}
		if seen[string(raw)] {
			return nil, fmt.Errorf("secrets: %s line %d names a duplicate recipient: %w",
				path, line, ErrBadRecipients)
		}
		seen[string(raw)] = true
		out = append(out, raw)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("secrets: %s authorizes nobody: %w", path, ErrBadRecipients)
	}
	return out, nil
}

// AddRecipient appends pub (a `pulse-pub-…` shareable key) to the recipients
// file, creating it with a header comment when absent.
//
// Idempotent on repeats: adding a listed key is a no-op that still validates
// the file, so a setup script can run twice without forking the set. The
// file is created 0644 — it holds public keys, and a 0600 recipients file
// would break every teammate's read in a shared checkout.
func AddRecipient(path, pub string) error {
	pub = strings.TrimSpace(pub)
	raw, err := DecodePub(IdentityPubPrefix, pub)
	if err != nil {
		return fmt.Errorf("secrets: refusing to add %q: %w", pub, err)
	}
	fresh := false
	if _, statErr := os.Stat(path); statErr == nil {
		existing, err := ParseRecipients(path)
		if err != nil {
			return err
		}
		for _, e := range existing {
			if bytes.Equal(e, raw) {
				return nil // already listed: no-op, file untouched
			}
		}
	} else {
		fresh = true
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("secrets: open %s: %w", path, err)
	}
	defer f.Close()
	if fresh {
		if _, err := fmt.Fprintln(f, "# pulse.recipients: authorized vault recipients, one pulse-pub-... key per line"); err != nil {
			return fmt.Errorf("secrets: write %s: %w", path, err)
		}
	}
	if _, err := fmt.Fprintln(f, pub); err != nil {
		return fmt.Errorf("secrets: write %s: %w", path, err)
	}
	return nil
}
