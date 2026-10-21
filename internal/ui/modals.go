package ui

import (
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// This file holds the composer's satellite modals: cURL import, vault seal,
// and new-file creation. Each is a tiny Elm sub-model — Open/Close/Active,
// Update on keys, View — owned by the root Model, which gives them modal
// precedence in updateKey. None performs I/O: submit hands a value back and
// the root dispatches a background tea.Cmd (AGENTS.md §3.2).

// ImportModel accepts a pasted curl command. A textarea (not a textinput)
// holds the paste because real-world copies span lines on backslash
// continuations; enter submits (the root intercepts it before the textarea
// can insert a newline), esc cancels.
type ImportModel struct {
	active bool
	paste  textarea.Model
}

// NewImportModel returns a closed import modal.
func NewImportModel() ImportModel {
	ta := textarea.New()
	ta.Placeholder = "curl -H 'Authorization: Bearer ...' https://..."
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 1 << 20
	return ImportModel{paste: ta}
}

// Open shows the modal with a blank buffer: a stale paste resubmitted
// would re-import a request the user already handled.
func (m *ImportModel) Open() {
	m.active = true
	m.paste.SetValue("")
	m.paste.Focus()
}

// Close hides the modal without importing.
func (m *ImportModel) Close() {
	m.active = false
	m.paste.Blur()
}

// Active reports visibility.
func (m ImportModel) Active() bool { return m.active }

// Text exposes the paste for submit.
func (m ImportModel) Text() string { return m.paste.Value() }

// Update routes a key to the paste area.
func (m *ImportModel) Update(msg tea.KeyMsg) { m.paste, _ = m.paste.Update(msg) }

// View renders the paste area with its submit hint.
func (m ImportModel) View(w int, ascii bool) string {
	body := styleAccent.Render("paste curl:") + "\n" + m.paste.View() + "\n" +
		styleMuted.Render("enter import   esc cancel   secrets become {{PLACEHOLDERS}}")
	return body
}

// VaultModel seals one token without leaving the TUI: a name field and a
// value field, tab toggles, enter seals, esc cancels. The value is never
// echoed back in any toast or view — success reports the name only.
type VaultModel struct {
	active int // 0 closed, 1 name, 2 value
	name   textinput.Model
	value  textinput.Model
}

// NewVaultModel returns a closed vault modal. The value field masks with
// echo asterisks: shoulder-surfing a seal over screen-share is a leak no
// redactor can scrub, because it never enters the process.
func NewVaultModel() VaultModel {
	mk := func(ph string) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = ph
		ti.Prompt = ""
		return ti
	}
	val := mk("paste the token (never echoed)")
	val.EchoMode = textinput.EchoPassword
	return VaultModel{name: mk("SECRET_NAME"), value: val}
}

// Open shows the modal, optionally prefilling the name (the error doctor
// passes the missing variable so the fix is one paste, not two).
func (m *VaultModel) Open(prefill string) {
	m.active = 1
	m.name.SetValue(prefill)
	m.value.SetValue("")
	m.name.Focus()
	m.value.Blur()
}

// Close hides the modal and scrubs the value buffer: a cancelled seal
// must not linger in memory past its use.
func (m *VaultModel) Close() {
	m.active = 0
	m.name.Blur()
	m.value.Blur()
	m.value.SetValue("")
}

// Active reports visibility.
func (m VaultModel) Active() bool { return m.active != 0 }

// OnValue reports whether the value field holds focus.
func (m VaultModel) OnValue() bool { return m.active == 2 }

// Toggle moves between name and value.
func (m *VaultModel) Toggle() {
	if m.active == 1 {
		m.active = 2
		m.name.Blur()
		m.value.Focus()
	} else if m.active == 2 {
		m.active = 1
		m.value.Blur()
		m.name.Focus()
	}
}

// Credentials exposes the typed pair for the seal command.
func (m VaultModel) Credentials() (name, value string) {
	return m.name.Value(), m.value.Value()
}

// Update routes a key to the focused field.
func (m *VaultModel) Update(msg tea.KeyMsg) {
	if m.active == 2 {
		m.value, _ = m.value.Update(msg)
	} else {
		m.name, _ = m.name.Update(msg)
	}
}

// View renders both fields with the focused one accented.
func (m VaultModel) View(w int, ascii bool) string {
	nameRow, valRow := m.name.View(), m.value.View()
	if m.active == 1 {
		nameRow = styleAccent.Render("name : ") + nameRow
		valRow = styleMuted.Render("value: ") + valRow
	} else {
		nameRow = styleMuted.Render("name : ") + nameRow
		valRow = styleAccent.Render("value: ") + valRow
	}
	body := styleAccent.Render("seal a secret:") + "\n" + nameRow + "\n" + valRow + "\n" +
		styleMuted.Render("enter seal   tab next field   esc cancel   stored in keychain, else memory-only")
	return body
}

// NewFileModel names a new collection file. Enter creates it with one
// untitled request; esc cancels. Creation goes through the injected file
// saver (workspace jail + atomic write), never a direct create.
type NewFileModel struct {
	active bool
	name   textinput.Model
}

// NewNewFileModel returns a closed new-file modal.
func NewNewFileModel() NewFileModel {
	ti := textinput.New()
	ti.Placeholder = "requests.http"
	ti.Prompt = ""
	return NewFileModel{name: ti}
}

// Open shows the modal with the default filename selected.
func (m *NewFileModel) Open() {
	m.active = true
	m.name.SetValue("requests.http")
	m.name.Focus()
}

// Close hides the modal.
func (m *NewFileModel) Close() {
	m.active = false
	m.name.Blur()
}

// Active reports visibility.
func (m NewFileModel) Active() bool { return m.active }

// Filename exposes the typed name, defaulting an empty field to the
// placeholder rather than failing on a blank the user never touched.
func (m NewFileModel) Filename() string {
	if v := m.name.Value(); v != "" {
		return v
	}
	return "requests.http"
}

// Update routes a key to the filename field.
func (m *NewFileModel) Update(msg tea.KeyMsg) { m.name, _ = m.name.Update(msg) }

// View renders the filename field with its hint.
func (m NewFileModel) View(w int, ascii bool) string {
	body := styleAccent.Render("new collection file:") + "\n" + m.name.View() + "\n" +
		styleMuted.Render("enter create   esc cancel   created inside the workspace root")
	return body
}
