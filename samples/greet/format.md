# `greet` output formatter

The `greet` command supports three output modes, selected with the `--format` flag.

## Modes

### `plain` (default)

Prints a human-readable greeting to stdout.

```
$ greet --name Alice
Hello, Alice!
```

### `loud`

Uppercases the entire greeting string before printing.

```
$ greet --name Alice --format loud
HELLO, ALICE!
```

### `json`

Emits a single JSON object with a `message` key. Suitable for scripting and
machine consumption.

```
$ greet --name Alice --format json
{"message":"Hello, Alice!"}
```

## Flag reference

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--format` | `string` | `plain` | Output format: `plain`, `loud`, or `json`. |
| `--name` | `string` | `World` | Name to include in the greeting. |

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Greeting written successfully. |
| `1` | Invalid `--format` value or other usage error. |
