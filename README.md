# container-app-lightpanda

A Privasys container app that wraps the [Lightpanda](https://lightpanda.io) headless browser as an AI tool (MCP server).

## Overview

This container exposes the Lightpanda headless browser as MCP-compatible AI tools that can be deployed to Enclave OS Virtual (TDX) enclaves. AI agents can discover and invoke these tools through the Privasys platform with full hardware attestation.

## Tools

| Tool | Description | Endpoint |
|------|-------------|----------|
| `browse` | Fetch a web page and return its content as markdown or HTML | `POST /browse` |

### `browse`

Fetches a web page using Lightpanda's headless browser engine and returns the content in the requested format.

**Request:**
```json
{
  "url": "https://example.com",
  "format": "markdown"
}
```

**Response:**
```json
{
  "url": "https://example.com",
  "format": "markdown",
  "content": "# Example Domain\n\nThis domain is for use in illustrative examples..."
}
```

**Parameters:**
- `url` (string, required) — The URL to fetch. Must start with `http://` or `https://`.
- `format` (string, optional) — Output format: `"markdown"` (default) or `"html"`.

## Container MCP Manifest

The `privasys.json` file declares the available AI tools following the Privasys container MCP standard. This manifest is provided when creating the app on the platform and enables:

- **AI Tools (MCP)** — AI agents discover and invoke tools via the MCP protocol
- **API Testing** — Developers can test tools directly from the developer portal

## Building

```bash
docker build -t container-app-lightpanda .
```

## Running Locally

```bash
docker run -p 8080:8080 container-app-lightpanda
```

Test the browse tool:
```bash
curl -X POST http://localhost:8080/browse \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com", "format": "markdown"}'
```

## Deploying to Privasys

1. Push the Docker image to a registry
2. Create a container app on the platform with the `privasys.json` manifest
3. Deploy to a TDX enclave

## License

AGPL-3.0 — See [LICENSE](LICENSE) for details.
