# LLM API Proxy — Documentation

> All your LLM providers in one central place.

Welcome to the LLM API Proxy documentation. Use the guides below to get started, configure your setup, and learn about advanced features.

## Getting Started

| Guide                                 | Description                                        |
| ------------------------------------- | -------------------------------------------------- |
| [Getting Started](getting-started.md) | Install, configure, and run the proxy in 5 minutes |
| [Docker Deployment](docker.md)        | Run with Docker Compose for production             |

## Configuration

| Guide                                       | Description                                                           |
| ------------------------------------------- | --------------------------------------------------------------------- |
| [Configuration Reference](configuration.md) | Complete YAML config options                                          |
| [Provider Setup Guides](providers.md)       | Backend-specific setup for OpenRouter, Z.ai, Copilot, Codex, and more |

## Features

| Guide                                       | Description                                                        |
| ------------------------------------------- | ------------------------------------------------------------------ |
| [API Reference](api.md)                     | OpenAI-compatible endpoints, Anthropic Messages API, Responses API |
| [Routing & Failover](routing.md)            | Priority, round-robin, race, and staggered-race strategies         |
| [Circuit Breaker](circuit-breaker.md)       | Automatic backend suspension on rate limits                        |
| [Identity Spoofing](identity.md)            | Make requests look like they came from CLI tools                   |
| [Chat & Playground](chat-and-playground.md) | Persistent chat sessions, in-browser playground                    |
| [Authentication & Users](authentication.md) | API key auth, web UI sessions, user management CLI                 |

## Development

| Guide                               | Description                                      |
| ----------------------------------- | ------------------------------------------------ |
| [Development Guide](development.md) | Build, test, project structure, and contributing |
