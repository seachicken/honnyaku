# Honnyaku

A language server that translates your text with AI as you write.

Honnyaku automatically translates your text after a brief pause in editing, bringing translation into your editor through the Language Server Protocol (LSP).

## Motivation

Translating while writing often means copying text into another tool and repeating the process after every revision. Honnyaku aims to make translation part of the writing flow, with automatic updates as your file changes.

## Status

In development. The server currently supports the LSP lifecycle over standard input/output. AI translation is not implemented yet.

## Development

Run the server:

```sh
go run .
```

Build an executable for your editor's LSP client:

```sh
go build -o honnyaku .
```

The server uses standard input/output for LSP messages and standard error for logs.

## FAQ

### Why the name "Honnyaku"?

Honnyaku is named after "Honyaku Konnyaku," a gadget from Doraemon that lets you understand other languages.
