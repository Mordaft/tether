# tether

`tether` es un servidor CLI/TUI multiplataforma escrito en Go que actúa como *side-car* y puente de ejecución de terminales (PTY) para sistemas agénticos multi-agente (como `localloop`, IDEs y sub-agentes) mediante **MCP sobre HTTP / SSE**.

## Arquitectura

```text
tether/
├── cmd/tether/
│   └── main.go               # Punto de entrada, flags CLI (--port, --ttl) y orquestación
├── internal/
│   ├── bus/                  # Event Bus interno (Pub/Sub con canales Go) y cola FIFO de consentimientos
│   ├── env/                  # Detección de OS, shells y validación de rutas
│   ├── auth/                 # Handshake POST /auth/connect, tokens HMAC con TTL y middleware HTTP
│   ├── guard/                # Command Guard con parser AST (mvdan.cc/sh/v3/syntax)
│   ├── pty/                  # Pool de sesiones PTY aislado por AgentID + RingBuffer + limpieza ANSI
│   ├── mcp/                  # Servidor MCP HTTP/SSE (mark3labs/mcp-go) y handlers de Tools/Resources
│   └── tui/                  # Aplicación Bubbletea (pestañas por agente + panel común de consentimientos)
├── go.mod
└── README.md
```
