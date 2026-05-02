package main

import (
	"bufio"
	"catchat/shared"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// ── Terminal helpers ──────────────────────────────────────────────────────────

func termSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80, 24
	}
	return w, h
}

func moveCursor(row, col int) { fmt.Printf("\033[%d;%dH", row, col) }
func clearLine()              { fmt.Print("\033[2K") }
func hideCursor()             { fmt.Print("\033[?25l") }
func showCursor()             { fmt.Print("\033[?25h") }
func clearScreen()            { fmt.Print("\033[2J\033[H") }

func hline(w int) string     { return strings.Repeat("═", w) }
func boxTop(w int) string    { return "╔" + hline(w-2) + "╗" }
func boxBottom(w int) string { return "╚" + hline(w-2) + "╝" }
func boxMid(w int) string    { return "╠" + hline(w-2) + "╣" }

func boxRow(content string, w int) string {
	visible := stripANSI(content)
	pad := w - 2 - len([]rune(visible))
	if pad < 0 {
		runes := []rune(content)
		if len(runes) > w-2 {
			content = string(runes[:w-2])
		}
		pad = 0
	}
	return "║" + content + strings.Repeat(" ", pad) + "║"
}

func stripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}

// ── Server TUI struct ─────────────────────────────────────────────────────────

type serverTUIState struct {
	hub     *Hub
	port    string
	localIP string
	mu      sync.Mutex

	chatLines  []string
	inputBuf   string
	typingLine string

	dashTab    bool
	dashSel    int
	showBanned bool
}

func newServerTUI(hub *Hub, port, localIP string) *serverTUIState {
	return &serverTUIState{hub: hub, port: port, localIP: localIP}
}

func (s *serverTUIState) addChatLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatLines = append(s.chatLines, line)
	if len(s.chatLines) > 2000 {
		s.chatLines = s.chatLines[len(s.chatLines)-2000:]
	}
}

// clampDashSel ensures dashSel is within bounds for the current list.
// Must be called with s.mu held.
func (s *serverTUIState) clampDashSel(listLen int) {
	if listLen == 0 {
		s.dashSel = 0
		return
	}
	if s.dashSel >= listLen {
		s.dashSel = listLen - 1
	}
	if s.dashSel < 0 {
		s.dashSel = 0
	}
}

func (s *serverTUIState) run() {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)
	defer showCursor()

	hideCursor()
	clearScreen()

	// Drain server inbox into chat lines.
	go func() {
		for line := range s.hub.serverInbox {
			tag, payload, ok := shared.Decode(line)
			if !ok {
				s.addChatLine(line)
				s.render()
				continue
			}
			switch tag {
			case shared.TagMsg, shared.TagSys:
				m, ok := shared.ParseWireMessage(payload)
				if ok {
					s.addChatLine(m.Format())
				}
			case shared.TagTyp:
				s.mu.Lock()
				s.typingLine = fmt.Sprintf(">> %s is typing...", payload)
				s.mu.Unlock()
			case shared.TagClr:
				s.mu.Lock()
				s.typingLine = ""
				s.mu.Unlock()
			}
			s.render()
		}
	}()

	// Dashboard refresh — throttled to avoid flooding.
	dashCh := make(chan struct{}, 1)
	go func() {
		for range s.hub.dashRefresh {
			select {
			case dashCh <- struct{}{}:
			default:
			}
		}
	}()
	go func() {
		for range dashCh {
			if s.dashTab {
				s.render()
			}
		}
	}()

	// Typing idle clear.
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.mu.Lock()
			changed := s.typingLine != ""
			s.typingLine = ""
			s.mu.Unlock()
			if changed {
				s.render()
			}
		}
	}()

	s.render()

	inReader := bufio.NewReader(os.Stdin)
	for {
		b, err := inReader.ReadByte()
		if err != nil {
			break
		}

		switch b {
		case 9: // Tab — toggle chat / dashboard
			s.mu.Lock()
			s.dashTab = !s.dashTab
			s.dashSel = 0
			s.showBanned = false
			s.mu.Unlock()
			s.render()

		case 13, 10: // Enter
			s.mu.Lock()
			input := strings.TrimSpace(s.inputBuf)
			s.inputBuf = ""
			s.mu.Unlock()

			if s.dashTab {
				s.dashAction(input)
			} else if input != "" {
				s.handleInput(input)
			}
			s.render()

		case 127, 8: // Backspace
			s.mu.Lock()
			if len(s.inputBuf) > 0 {
				runes := []rune(s.inputBuf)
				s.inputBuf = string(runes[:len(runes)-1])
			}
			s.mu.Unlock()
			s.render()

		case 27: // Escape sequence (arrow keys)
			next, _ := inReader.ReadByte()
			if next == '[' {
				arrow, _ := inReader.ReadByte()
				if s.dashTab {
					s.mu.Lock()
					var listLen int
					if s.showBanned {
						listLen = len(s.hub.BlacklistedIPs())
					} else {
						listLen = len(s.hub.connectedList())
					}
					switch arrow {
					case 'A': // up
						if s.dashSel > 0 {
							s.dashSel--
						}
					case 'B': // down
						if listLen > 0 && s.dashSel < listLen-1 {
							s.dashSel++
						}
					}
					s.mu.Unlock()
					s.render()
				}
			}

		case 3: // Ctrl+C
			shutdownMsg := shared.Message{
				Timestamp: time.Now(),
				IsSystem:  true,
				Text:      "CatChat server is shutting down. Goodbye!",
			}
			s.hub.addHistory(shutdownMsg)
			s.hub.broadcast(shared.Encode(shared.TagSys, shutdownMsg.Wire()), nil)
			time.Sleep(400 * time.Millisecond)
			term.Restore(int(os.Stdin.Fd()), oldState)
			showCursor()
			clearScreen()
			fmt.Println("CatChat server stopped.")
			os.Exit(0)

		default:
			if b >= 32 && b < 127 {
				// 'v'/'V' in dashboard toggles banned view.
				if s.dashTab && (b == 'v' || b == 'V') {
					s.mu.Lock()
					s.showBanned = !s.showBanned
					s.dashSel = 0
					s.mu.Unlock()
					s.render()
					continue
				}
				s.mu.Lock()
				if len([]rune(s.inputBuf)) < shared.MaxMessageLen {
					s.inputBuf += string(b)
				}
				s.mu.Unlock()
				s.render()
			}
		}
	}
}

func (s *serverTUIState) handleInput(input string) {
	switch input {
	case "/list":
		clients := s.hub.connectedList()
		s.addChatLine(fmt.Sprintf("%s>> Connected users (%d):%s",
			shared.SysColour, len(clients)+1, shared.Reset))
		s.addChatLine(fmt.Sprintf("   %s%s%s",
			s.hub.serverClient.colour, s.hub.serverClient.name, shared.Reset))
		for _, c := range clients {
			s.addChatLine(fmt.Sprintf("   %s%s%s",
				c.colour, c.name, shared.Reset))
		}

	case "/history":
		s.hub.mu.RLock()
		msgs := s.hub.history.All()
		s.hub.mu.RUnlock()
		s.addChatLine(fmt.Sprintf("%s>> Full history (%d messages):%s",
			shared.SysColour, len(msgs), shared.Reset))
		for _, m := range msgs {
			s.addChatLine(m.Format())
		}

	default:
		if len([]rune(input)) > shared.MaxMessageLen {
			s.addChatLine(shared.SysColour + ">> Message too long (max 500 chars)" + shared.Reset)
			return
		}
		m := shared.Message{
			Timestamp: time.Now(),
			Sender:    s.hub.serverClient.name,
			Colour:    s.hub.serverClient.colour,
			Text:      input,
		}
		s.hub.addHistory(m)
		s.hub.broadcast(shared.Encode(shared.TagMsg, m.Wire()), nil)
	}
}

func (s *serverTUIState) dashAction(input string) {
	s.mu.Lock()
	sel := s.dashSel
	showBanned := s.showBanned
	s.mu.Unlock()

	if showBanned {
		ips := s.hub.BlacklistedIPs()
		if sel < len(ips) {
			ip := ips[sel]
			s.hub.Unblacklist(ip)
			// Clamp selection after removal.
			s.mu.Lock()
			s.clampDashSel(len(ips) - 1)
			s.mu.Unlock()
			s.addChatLine(fmt.Sprintf("%s>> Unblacklisted IP: %s%s",
				shared.AdminColour, ip, shared.Reset))
		}
		return
	}

	clients := s.hub.connectedList()
	if sel >= len(clients) {
		return
	}

	mode := strings.ToLower(strings.TrimSpace(input))
	c := clients[sel]
	// Clamp selection anticipating the client list will shrink by one.
	s.mu.Lock()
	s.clampDashSel(len(clients) - 1)
	s.mu.Unlock()

	if mode == "ban" || mode == "b" {
		s.hub.Ban(c)
	} else {
		// Default action (including 'k') is kick.
		s.hub.Kick(c)
	}
}

// ── Render ────────────────────────────────────────────────────────────────────

func (s *serverTUIState) render() {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, h := termSize()
	if w < 40 || h < 10 {
		return
	}

	if s.dashTab {
		s.renderDashboard(w, h)
	} else {
		s.renderChat(w, h)
	}
}

func (s *serverTUIState) renderChat(w, h int) {
	// Title: "CatChat Server ─ <colour>name<reset> ─ <ip> ─ port <port>"
	title := fmt.Sprintf(" CatChat Server ─ %s%s%s ─ %s ─ port %s ",
		s.hub.serverClient.colour, s.hub.serverClient.name, shared.Reset,
		s.localIP, s.port)
	titleVis := fmt.Sprintf(" CatChat Server ─ %s ─ %s ─ port %s ",
		s.hub.serverClient.name, s.localIP, s.port)
	titlePad := w - 2 - len([]rune(titleVis))
	if titlePad < 0 {
		titlePad = 0
	}

	moveCursor(1, 1)
	fmt.Print(shared.Bold + "╔" + title + strings.Repeat("═", titlePad) + "╗" + shared.Reset)

	chatH := h - 6
	if chatH < 1 {
		chatH = 1
	}

	lines := s.chatLines
	start := 0
	if len(lines) > chatH {
		start = len(lines) - chatH
	}
	for i := 0; i < chatH; i++ {
		moveCursor(2+i, 1)
		clearLine()
		if start+i < len(lines) {
			l := lines[start+i]
			vis := stripANSI(l)
			pad := w - 2 - len([]rune(vis))
			if pad < 0 {
				pad = 0
			}
			fmt.Print("║" + l + strings.Repeat(" ", pad) + "║")
		} else {
			fmt.Print(boxRow("", w))
		}
	}

	moveCursor(2+chatH, 1)
	fmt.Print(boxMid(w))
	moveCursor(3+chatH, 1)
	typ := s.typingLine
	if typ == "" {
		typ = shared.Dim + "" + shared.Reset
	}
	fmt.Print(boxRow(shared.Dim+typ+shared.Reset, w))

	moveCursor(4+chatH, 1)
	fmt.Print(boxMid(w))
	moveCursor(5+chatH, 1)
	promptStr := "> " + s.inputBuf
	vis := stripANSI(promptStr)
	pad := w - 2 - len([]rune(vis))
	if pad < 0 {
		pad = 0
	}
	fmt.Print("║" + promptStr + shared.Blink + "█" + shared.Reset + strings.Repeat(" ", max(0, pad-1)) + "║")

	moveCursor(6+chatH, 1)
	clients := len(s.hub.connectedList())
	status := fmt.Sprintf(" [Tab] Admin Dashboard  [Ctrl+C] Quit  ─  %d/%d users connected ",
		clients+1, shared.MaxClients)
	fmt.Print(shared.SysColour + "╚" + truncate(status, w-2) +
		strings.Repeat("═", max(0, w-2-len([]rune(status)))) + "╝" + shared.Reset)
}

func (s *serverTUIState) renderDashboard(w, h int) {
	clearScreen()
	clients := s.hub.connectedList()
	bannedIPs := s.hub.BlacklistedIPs()

	title := fmt.Sprintf(" CatChat ─ Admin Dashboard ─ %s ", s.localIP)
	titlePad := w - 2 - len([]rune(title))
	if titlePad < 0 {
		titlePad = 0
	}
	moveCursor(1, 1)
	fmt.Print(shared.Bold + shared.AdminColour + "╔" + title + strings.Repeat("═", titlePad) + "╗" + shared.Reset)

	row := 2
	if !s.showBanned {
		// Clamp selection to current client list.
		s.clampDashSel(len(clients))

		moveCursor(row, 1)
		fmt.Print(boxRow(fmt.Sprintf("%s>> Connected Users (%d/%d)  [K]ick  [B]an  [V]iew bans  [Tab] chat%s",
			shared.AdminColour, len(clients)+1, shared.MaxClients, shared.Reset), w))
		row++
		moveCursor(row, 1)
		fmt.Print(boxMid(w))
		row++

		// Server pseudo-client (always shown, not selectable).
		moveCursor(row, 1)
		line := fmt.Sprintf("  %s%-16s%s  %-8s  %-16s  %s",
			s.hub.serverClient.colour, s.hub.serverClient.name, shared.Reset,
			"[server]", s.localIP, s.hub.serverClient.Duration())
		fmt.Print(boxRow(line, w))
		row++

		for i, c := range clients {
			moveCursor(row, 1)
			cursor := "  "
			if i == s.dashSel {
				cursor = shared.Bold + "> " + shared.Reset
			}
			line := fmt.Sprintf("%s%s%-16s%s  %-16s  %s",
				cursor, c.colour, c.name, shared.Reset, c.ip, c.Duration())
			fmt.Print(boxRow(line, w))
			row++
		}
		if len(clients) == 0 {
			moveCursor(row, 1)
			fmt.Print(boxRow("  No connected clients.", w))
			row++
		}

		moveCursor(row, 1)
		fmt.Print(boxMid(w))
		row++
		moveCursor(row, 1)
		fmt.Print(boxRow("  Type 'k' + Enter = kick  |  'b' + Enter = ban  |  [V] view bans", w))
		row++
		moveCursor(row, 1)
		promptStr := "> " + s.inputBuf
		fmt.Print(boxRow(promptStr+shared.Blink+"█"+shared.Reset, w))
		row++
		moveCursor(row, 1)
		fmt.Print(boxMid(w))
		row++
		moveCursor(row, 1)
		fmt.Print(boxRow(fmt.Sprintf("%s>> Banned IPs: %d  ─  press [V] to manage%s",
			shared.AdminColour, len(bannedIPs), shared.Reset), w))
		row++

	} else {
		// Clamp selection to banned list.
		s.clampDashSel(len(bannedIPs))

		moveCursor(row, 1)
		fmt.Print(boxRow(fmt.Sprintf("%s>> Banned IPs (%d)  ─  Select + Enter to unban  [V] back  [Tab] chat%s",
			shared.AdminColour, len(bannedIPs), shared.Reset), w))
		row++
		moveCursor(row, 1)
		fmt.Print(boxMid(w))
		row++

		for i, ip := range bannedIPs {
			moveCursor(row, 1)
			cursor := "  "
			if i == s.dashSel {
				cursor = shared.Bold + "> " + shared.Reset
			}
			fmt.Print(boxRow(cursor+ip, w))
			row++
		}
		if len(bannedIPs) == 0 {
			moveCursor(row, 1)
			fmt.Print(boxRow("  No banned IPs.", w))
			row++
		}
	}

	// Fill remaining rows.
	for row <= h-1 {
		moveCursor(row, 1)
		fmt.Print(boxRow("", w))
		row++
	}

	moveCursor(h, 1)
	status := " [↑↓] Navigate  [Enter] Action  [V] Toggle view  [Tab] Chat  [Ctrl+C] Quit "
	fmt.Print(shared.AdminColour + "╚" + truncate(status, w-2) +
		strings.Repeat("═", max(0, w-2-len([]rune(status)))) + "╝" + shared.Reset)
}