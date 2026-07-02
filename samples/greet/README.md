# greet

A minimal command-line tool that prints a configurable greeting.

## Usage

```
greet [--name <name>] [--greeting <greeting>]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | `World` | The name to greet. |
| `--greeting` | `Hello` | The greeting word to use. |

## Examples

```
$ greet
Hello, World!

$ greet --name Alice
Hello, Alice!

$ greet --name Bob --greeting Hi
Hi, Bob!
```

## Overview

`greet` demonstrates the minimal structure for a sample module:

- accepts named flags with sensible defaults
- formats and prints a single line of output (`<greeting>, <name>!`)
- exits with code `0` on success, non-zero on invalid input
