# `greet` usage guide

`greet` is a minimal command-line tool that prints a configurable greeting. It
demonstrates the standard pattern for a sample module: named flags with
sensible defaults, locale-aware output, and a machine-readable JSON mode.

## Synopsis

```
greet [--name <name>] [--locale <tag>] [--format <mode>]
```

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--name` | string | `World` | The name to include in the greeting. |
| `--locale` | string | `en` | BCP 47 language tag that selects the greeting phrase. |
| `--format` | string | `plain` | Output mode: `plain`, `loud`, or `json`. |

### Supported locales

| Tag | Greeting phrase |
|-----|-----------------|
| `en` | Hello |
| `es` | Hola |
| `fr` | Bonjour |
| `de` | Hallo |
| `ja` | こんにちは |

Unrecognised tags fall back to `en`.

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Greeting written successfully. |
| `1` | Invalid flag value or other usage error. |

## Example invocations

### Default greeting

```
$ greet
Hello, World!
```

### Custom name

```
$ greet --name Alice
Hello, Alice!
```

### Different locale

```
$ greet --name María --locale es
Hola, María!
```

### Loud (uppercase) output

```
$ greet --name Bob --format loud
HELLO, BOB!
```

### JSON output

```
$ greet --name Alice --format json
{"message":"Hello, Alice!"}
```

### Combining all flags

```
$ greet --name Claude --locale fr --format loud
BONJOUR, CLAUDE!
```

