package main

import (
	"bufio"
	"catchat/shared"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

func moveCursor(row, col int) { fmt.Printf("\033[%d;%dH", row, col) }
func clearLine()              { fmt.Print("\033[2K") }
func hideCursor()             { fmt.Print("\033[?25l") }
func showCursor()             { fmt.Print("\033[?25h") }
func clearScreen()            { fmt.Print("\033[2J\033[H") }

func termSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80, 24
	}
	return w, h
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

func hline(w int) string     { return strings.Repeat("═", w) }
func boxMid(w int) string    { return "╠" + hline(w-2) + "╣" }
func boxBottom(w int) string { return "╚" + hline(w-2) + "╝" }

func boxRow(content string, w int) string {
	vis := stripANSI(content)
	pad := w - 2 - len([]rune(vis))
	if pad < 0 {
		pad = 0
	}
	return "║" + content + strings.Repeat(" ", pad) + "║"
}

// ── Client TUI ────────────────────────────────────────────────────────────────

type clientTUI struct {
	conn       net.Conn
	reader     *bufio.Reader
	name       string
	colour     string
	serverAddr string

	mu         sync.Mutex
	chatLines  []string
	inputBuf   string
	typingLine string

	typingTimer *time.Timer
	typingSent  bool
}

func (c *clientTUI) addLine(line string) {
	c.mu.Lock()
	c.chatLines = append(c.chatLines, line)
	if len(c.chatLines) > 2000 {
		c.chatLines = c.chatLines[len(c.chatLines)-2000:]
	}
	c.mu.Unlock()
}

func (c *clientTUI) send(tag, payload string) {
	fmt.Fprint(c.conn, shared.Encode(tag, payload))
}

func (c *clientTUI) render() {
	c.mu.Lock()
	defer c.mu.Unlock()

	w, h := termSize()
	if w < 40 || h < 8 {
		return
	}

	title := fmt.Sprintf(" CatChat ─ %s%s%s ─ %s ",
		c.colour, c.name, shared.Reset, c.serverAddr)
	titleVis := fmt.Sprintf(" CatChat ─ %s ─ %s ", c.name, c.serverAddr)
	titlePad := w - 2 - len([]rune(titleVis))
	if titlePad < 0 {
		titlePad = 0
	}
	moveCursor(1, 1)
	fmt.Print(shared.Bold + shared.Colours[5].Code +
		"╔" + title + strings.Repeat("═", titlePad) + "╗" +
		shared.Reset)

	chatH := h - 6
	if chatH < 1 {
		chatH = 1
	}

	lines := c.chatLines
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
	typ := c.typingLine
	fmt.Print(boxRow(shared.Dim+typ+shared.Reset, w))

	moveCursor(4+chatH, 1)
	fmt.Print(boxMid(w))
	moveCursor(5+chatH, 1)
	prompt := "> " + c.inputBuf
	vis := stripANSI(prompt)
	pad := w - 2 - len([]rune(vis))
	if pad < 0 {
		pad = 0
	}
	fmt.Print("║" + prompt + shared.Blink + "█" + shared.Reset + strings.Repeat(" ", max(0, pad-1)) + "║")

	moveCursor(6+chatH, 1)
	status := " /list  /history  [Ctrl+C] Quit "
	fmt.Print(shared.Colours[5].Code + "╚" +
		truncate(status, w-2) +
		strings.Repeat("═", max(0, w-2-len([]rune(status)))) +
		"╝" + shared.Reset)
}

func (c *clientTUI) handleServerLine(raw string) {
	tag, payload, ok := shared.Decode(raw)
	if !ok {
		return
	}

	switch tag {
	case shared.TagMsg:
		m, ok := shared.ParseWireMessage(payload)
		if ok {
			c.addLine(m.Format())
		}
	case shared.TagSys:
		m, ok := shared.ParseWireMessage(payload)
		if ok {
			c.addLine(m.Format())
		}
	case shared.TagHistStart:
		c.addLine(shared.SysColour + ">> ─── History ───────────────────────────" + shared.Reset)
	case shared.TagHistEnd:
		c.addLine(shared.SysColour + ">> ─── End of history ───────────────────" + shared.Reset)
	case shared.TagCmd:
		// Each TagCmd carries exactly one display line now.
		// Keep the split as a safety net for any legacy multi-line payload.
		for _, line := range strings.Split(payload, "\n") {
			if line != "" {
				c.addLine(shared.SysColour + line + shared.Reset)
			}
		}
	case shared.TagTyp:
		c.mu.Lock()
		c.typingLine = fmt.Sprintf(">> %s is typing...", payload)
		c.mu.Unlock()
	case shared.TagClr:
		c.mu.Lock()
		c.typingLine = ""
		c.mu.Unlock()
	case shared.TagCtl:
		switch payload {
		case "KICKED":
			c.addLine(shared.AdminColour + ">> You have been kicked by the server." + shared.Reset)
		case "BANNED":
			c.addLine(shared.AdminColour + ">> You have been banned from this server." + shared.Reset)
		case "FULL":
			c.addLine(shared.SysColour + ">> Server is full. Try again later." + shared.Reset)
		case "MSG_TOO_LONG":
			c.addLine(shared.SysColour + ">> Message too long (max 500 characters)." + shared.Reset)
		case "HST_EMPTY":
			c.addLine(shared.SysColour + ">> No history available." + shared.Reset)
		}
	}
}

func (c *clientTUI) run() {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)
	defer showCursor()

	hideCursor()
	clearScreen()

	// Single read goroutine — shares the buffered reader from the handshake.
	go func() {
		sc := bufio.NewScanner(c.reader)
		for sc.Scan() {
			c.handleServerLine(sc.Text())
			c.render()
		}
		c.addLine(shared.SysColour + ">> Disconnected from server." + shared.Reset)
		c.render()
	}()

	c.typingTimer = time.AfterFunc(999*time.Hour, func() {
		c.mu.Lock()
		wasSent := c.typingSent
		c.typingSent = false
		c.mu.Unlock()
		if wasSent {
			c.send(shared.TagClr, "")
		}
	})

	// Clear stale typing indicator after 4 seconds of silence.
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			c.mu.Lock()
			changed := c.typingLine != ""
			c.typingLine = ""
			c.mu.Unlock()
			if changed {
				c.render()
			}
		}
	}()

	c.render()

	inReader := bufio.NewReader(os.Stdin)
	for {
		b, err := inReader.ReadByte()
		if err != nil {
			break
		}

		switch b {
		case 3: // Ctrl+C
			c.send(shared.TagClr, "")
			term.Restore(int(os.Stdin.Fd()), oldState)
			showCursor()
			clearScreen()
			fmt.Println("Disconnected from CatChat. Goodbye!")
			os.Exit(0)

		case 13, 10: // Enter
			c.mu.Lock()
			input := strings.TrimSpace(c.inputBuf)
			c.inputBuf = ""
			c.mu.Unlock()

			// Stop typing indicator.
			c.typingTimer.Reset(999 * time.Hour)
			c.mu.Lock()
			wasSent := c.typingSent
			c.typingSent = false
			c.mu.Unlock()
			if wasSent {
				c.send(shared.TagClr, "")
			}

			if input == "" {
				c.render()
				continue
			}

			switch input {
			case "/list", "/history":
				c.send(shared.TagQry, input)
			default:
				if len([]rune(input)) > shared.MaxMessageLen {
					c.addLine(shared.SysColour + ">> Message too long (max 500 characters)." + shared.Reset)
				} else {
					c.send(shared.TagMsg, input)
				}
			}
			c.render()

		case 127, 8: // Backspace
			c.mu.Lock()
			if len(c.inputBuf) > 0 {
				runes := []rune(c.inputBuf)
				c.inputBuf = string(runes[:len(runes)-1])
			}
			c.mu.Unlock()
			c.render()

		case 27: // Escape / arrow keys — consume and ignore
			inReader.ReadByte()
			inReader.ReadByte()

		default:
			if b >= 32 && b < 127 {
				c.mu.Lock()
				if len([]rune(c.inputBuf)) < shared.MaxMessageLen {
					c.inputBuf += string(b)
				}
				if !c.typingSent {
					c.typingSent = true
					go c.send(shared.TagTyp, "")
				}
				c.typingTimer.Reset(2 * time.Second)
				c.mu.Unlock()
				c.render()
			}
		}
	}
}

// ── Setup prompts ─────────────────────────────────────────────────────────────

func setupPrompts() (name, colourCode, serverAddr string) {
	reader := bufio.NewReader(os.Stdin)
	prompt := func(q string) string {
		fmt.Print("  >> " + q + ": ")
		line, _ := reader.ReadString('\n')
		return strings.TrimRight(line, "\r\n")
	}

	// Display name
	for {
		n := strings.TrimSpace(prompt("Your display name"))
		if n == "" {
			fmt.Println("  Name cannot be empty.")
			continue
		}
		if len([]rune(n)) > 9 {
			fmt.Println("  Name too long (max 9 chars).")
			continue
		}
		if strings.Contains(n, "|") {
			fmt.Println("  Name cannot contain '|' character.")
			continue
		}
		name = n
		break
	}

	// Colour
	fmt.Println()
	fmt.Println("  Choose a colour:")
	for i, c := range shared.Colours {
		fmt.Printf("    %d. %s%s%s\n", i+1, c.Code, c.Name, shared.Reset)
	}
	for {
		c := strings.TrimSpace(prompt("Colour (1-6)"))
		idx := -1
		for i, col := range shared.Colours {
			if c == fmt.Sprintf("%d", i+1) || strings.EqualFold(c, col.Name) {
				idx = i
				break
			}
		}
		if idx < 0 {
			fmt.Println("  Invalid choice, enter a number 1-6.")
			continue
		}
		colourCode = shared.Colours[idx].Code
		break
	}

	// Server address — detect local IP as a helpful default suggestion.
	fmt.Println()
	localIP := shared.GetLocalIP()
	ip := strings.TrimSpace(
		prompt(fmt.Sprintf("Server IP address (press enter for %s)", localIP)),
	)
	if ip == "" {
		ip = localIP
	}

	port := strings.TrimSpace(
		prompt(fmt.Sprintf("Server port (press enter for %s)", shared.DefaultPort)),
	)
	if port == "" {
		port = shared.DefaultPort
	}

	serverAddr = net.JoinHostPort(ip, port)
	return
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	clearScreen()
	fmt.Print(shared.Bold + shared.Colours[5].Code + shared.Splash + shared.Reset)
	fmt.Println(shared.SysColour + "  ══════════════════════════════════════════════════" + shared.Reset)
	fmt.Println("  CLIENT SETUP")
	fmt.Println(shared.SysColour + "  ══════════════════════════════════════════════════" + shared.Reset)
	fmt.Println()

	name, colour, serverAddr := setupPrompts()

	fmt.Println()
	fmt.Printf("  %s>> Connecting to %s...%s\n", shared.SysColour, serverAddr, shared.Reset)

	conn, err := tls.Dial("tcp", serverAddr, shared.ClientTLSConfig())
	if err != nil {
		fmt.Println()
		fmt.Printf("  %s>> Server not online or unreachable at %s%s\n", shared.SysColour, serverAddr, shared.Reset)
		fmt.Println("  >> Please check the IP address and port, or contact the server administrator.")
		fmt.Println()
		fmt.Print("  >> Press Enter to exit...")
		bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(1)
	}

	fmt.Printf("  %s>> Connected! (TLS encrypted)%s\n", shared.SysColour, shared.Reset)
	time.Sleep(600 * time.Millisecond)

	// ── Identity handshake ────────────────────────────────────────────────────
	reader := bufio.NewReader(conn)
	readLine := func() (string, error) {
		line, err := reader.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	sendLine := func(tag, payload string) {
		fmt.Fprint(conn, shared.Encode(tag, payload))
	}

handshake:
	for {
		raw, err := readLine()
		if err != nil {
			fmt.Println("  >> Connection lost during handshake.")
			os.Exit(1)
		}
		tag, payload, ok := shared.Decode(raw)
		if !ok {
			continue
		}
		switch tag {
		case shared.TagWho:
			switch {
			case payload == "NAME":
				sendLine(shared.TagWho, name)
			case payload == "NAME_TAKEN":
				fmt.Printf("  %s>> Name '%s' is already taken. Choose another: %s",
					shared.SysColour, name, shared.Reset)
				sc := bufio.NewScanner(os.Stdin)
				sc.Scan()
				name = strings.TrimSpace(sc.Text())
				sendLine(shared.TagWho, name)
			case payload == "NAME_EMPTY":
				fmt.Print("  >> Name cannot be empty: ")
				sc := bufio.NewScanner(os.Stdin)
				sc.Scan()
				name = strings.TrimSpace(sc.Text())
				sendLine(shared.TagWho, name)
			case payload == "NAME_LONG":
				fmt.Print("  >> Name too long (max 9 chars): ")
				sc := bufio.NewScanner(os.Stdin)
				sc.Scan()
				name = strings.TrimSpace(sc.Text())
				sendLine(shared.TagWho, name)
			case payload == "NAME_INVALID":
				fmt.Print("  >> Name contains invalid characters: ")
				sc := bufio.NewScanner(os.Stdin)
				sc.Scan()
				name = strings.TrimSpace(sc.Text())
				sendLine(shared.TagWho, name)
			case strings.HasPrefix(payload, "RETURNING|"):
				parts := strings.SplitN(payload, "|", 3)
				if len(parts) == 3 {
					name = parts[1]
					colour = parts[2]
					fmt.Printf("  %s>> Welcome back, %s%s%s!%s\n",
						shared.SysColour, colour, name, shared.Reset, shared.Reset)
					time.Sleep(600 * time.Millisecond)
				}
			case payload == "COLOUR":
				for i, c := range shared.Colours {
					if c.Code == colour {
						sendLine(shared.TagWho, fmt.Sprintf("%d", i+1))
						break
					}
				}
			case payload == "COLOUR_INVALID":
				sendLine(shared.TagWho, "1")
			}
		case shared.TagAck:
			parts := strings.SplitN(payload, "|", 3)
			if len(parts) == 3 && parts[0] == "OK" {
				name = parts[1]
				colour = parts[2]
			}
			break handshake
		case shared.TagCtl:
			switch payload {
			case "FULL":
				fmt.Println("\n  >> Server is full. Try again later.")
				os.Exit(1)
			case "BANNED":
				fmt.Println("\n  >> You have been banned from this server.")
				os.Exit(1)
			}
		}
	}

	// Pass the same buffered reader into the TUI to avoid losing buffered bytes.
	tui := &clientTUI{
		conn:       conn,
		reader:     reader,
		name:       name,
		colour:     colour,
		serverAddr: serverAddr,
	}
	tui.run()
}