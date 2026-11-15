# tether

`tether` es un servidor CLI/TUI multiplataforma escrito en **Go** que actúa como *side-car* y puente de ejecución de terminales (PTY) para sistemas agénticos multi-agente (como `localloop`, IDEs y sub-agentes).

Corre como una única instancia central en un puerto HTTP fijo (por defecto `127.0.0.1:7432`) y permite que múltiples agentes y sub-agentes se conecten simultáneamente mediante **MCP sobre HTTP / SSE (Streamable HTTP y SSE)**, manteniendo sus sesiones de terminal estrictamente aisladas por identidad de agente (`agent_id`) y acotadas a su `workspace_root` autorizado, todo supervisado en tiempo real por un operador humano a través de una interfaz **TUI de grado Neovim/LazyVim/Zellij** a 60 FPS.

---

## Estructura del Proyecto

```text
tether/
├── cmd/tether/
│   └── main.go               # Punto de entrada, flags CLI (--port, --ttl, --idle-timeout, --no-icons)
├── internal/
│   ├── bus/                  # Event Bus interno (Pub/Sub con canales Go) y cola FIFO de consentimientos
│   ├── env/                  # Detección de OS, arquitectura, shells instaladas y validación de rutas
│   ├── auth/                 # Handshake POST /auth/connect, tokens HMAC-SHA256 con TTL y middleware HTTP
│   ├── guard/                # Command Guard con parser AST (mvdan.cc/sh/v3/syntax) y seguimiento de CWD
│   ├── pty/                  # Pool PTY aislado por AgentID (go-pty) + Job Objects/PGID + RingBuffer + ANSI strip
│   ├── mcp/                  # Servidor MCP HTTP/SSE (mark3labs/mcp-go) y handlers de Tools/Resources
│   └── tui/                  # TUI estilo Neovim/LazyVim (Tether Night, 60 FPS, virtualización, floats)
│       ├── theme.go          # Paleta "Tether Night", glifos Nerd Font v3 + fallback --no-icons y Chroma
│       ├── model.go          # Estado principal Bubbletea, máquina modal (Normal/Terminal) y loop 60 FPS
│       ├── keys.go           # Keybindings estilo Vim, Leader Key (Space), timer Which-Key (300ms) y raw PTY bytes
│       ├── viewport.go       # Renderizado virtualizado del RingBuffer + búsqueda (/, n/N) + yank (y)
│       ├── overlays.go       # Compositor de ventanas flotantes (Consent Modal, Fuzzy Switcher, Inspect, Which-Key)
│       └── statusline.go     # Top tab bar de 1 línea y statusline inferior estilo lualine
├── go.mod
└── README.md
```

---

## Uso y Flags CLI

```bash
# Compilar binario
go build -o tether ./cmd/tether

# Ejecutar con iconos Nerd Fonts v3 (por defecto en 127.0.0.1:7432, TTL 30m)
./tether --port 7432 --ttl 30m --idle-timeout 3s

# Ejecutar en terminales sin Nerd Fonts (fallback ASCII: [AG], [DIR], [SH], [PS], [PERM]...)
./tether --no-icons
```

---

## Arquitectura Modal y Atajos de Teclado (Estilo Neovim / LazyVim)

### Modo `NORMAL` (Por defecto — badge `[ NORMAL ]`)
* **Navegación entre pestañas:** `h` / `l` o `[` / `]` (también `Tab` / `Shift+Tab`).
* **Scroll y lectura libre:** `j` / `k`, `Ctrl+u` / `Ctrl+d`, `g` (inicio del buffer + pausa *auto-scroll*) y `G` (final del buffer + reactiva *auto-scroll*).
* **Búsqueda en buffer (`/`)**: Busca patrones dentro del historial de la pestaña activa y salta entre coincidencias con `n` y `N`.
* **Yank (`y`)**: Copia el último bloque de salida al portapapeles del sistema (`atotto/clipboard`) mostrando una notificación toast.
* **Aprobaciones rápidas (Modal de Consentimientos):** `[a]` Aprobar 1 vez · `[s]` Aprobar en esta sesión · `[d]` Denegar · `[Tab]` Siguiente en cola.
* **Entrar a Modo `TERMINAL`:** `i` o `Enter`.

### Modo `TERMINAL` / `INSERT` (Intervención directa — badge `[ TERMINAL ]`)
* Todas las pulsaciones de teclado se envían en crudo (`raw bytes`) directamente a la PTY de la pestaña activa.
* **Salida segura:** Pulsar `Esc` dos veces rápidas (`Esc Esc`) devuelve el control al modo `NORMAL`.

### Leader Key (`Space`) y Menú `which-key` (300ms)
* `Space + Space` o `Space + t` → **Session Switcher** (Fuzzy Finder en vivo estilo Telescope con `sahilm/fuzzy`).
* `Space + a` → **Approvals Queue** (Abrir/alternar el modal flotante de consentimientos pendientes).
* `Space + o` → **Inspect Output** (Visor flotante al 85% con resaltado de sintaxis `chroma`).
* `Space + w` → **Workspaces & Agents** (Agentes conectados, `agent_id`, rutas autorizadas y TTL restante).
* `Space + z` → **Zen / Zoom Mode** (Maximiza el viewport al 100% ocultando las barras superior e inferior).
* `Space + k` → **Kill Process Group** (Mata el árbol completo de subprocesos de la pestaña activa previa confirmación).
