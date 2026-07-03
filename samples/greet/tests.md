# Integration Test Plan — `greet`

## Overview

This document describes the integration test plan for the `greet` CLI command.
Tests cover three dimensions:

1. **Flags** — presence, absence, mutual exclusion, and error handling
2. **Locales** — correct greeting strings per locale, fallback behaviour
3. **Output formats** — plain text, JSON, and YAML

Tests are black-box: invoke the compiled binary and assert on stdout, stderr,
and exit code. Each test case is independent; no shared state between cases.

---

## 1. Flags

### 1.1 `--name`

| ID | Command | Expected stdout | Exit code |
|----|---------|-----------------|-----------|
| F-01 | `greet --name Alice` | `Hello, Alice!` | 0 |
| F-02 | `greet --name "Ada Lovelace"` | `Hello, Ada Lovelace!` | 0 |
| F-03 | `greet` (no `--name`) | `Hello, World!` | 0 |
| F-04 | `greet --name ""` | non-empty error on stderr; usage hint | 1 |
| F-05 | `greet --name Alice --name Bob` (duplicate) | non-empty error on stderr | 1 |

### 1.2 `--loud`

| ID | Command | Expected stdout | Exit code |
|----|---------|-----------------|-----------|
| F-06 | `greet --name Alice --loud` | `HELLO, ALICE!` | 0 |
| F-07 | `greet --loud` (no `--name`) | `HELLO, WORLD!` | 0 |

### 1.3 `--times`

| ID | Command | Expected stdout | Exit code |
|----|---------|-----------------|-----------|
| F-08 | `greet --name Alice --times 3` | three greeting lines | 0 |
| F-09 | `greet --name Alice --times 1` | one greeting line | 0 |
| F-10 | `greet --name Alice --times 0` | non-empty error on stderr | 1 |
| F-11 | `greet --name Alice --times -1` | non-empty error on stderr | 1 |
| F-12 | `greet --name Alice --times abc` | non-empty error on stderr | 1 |

### 1.4 `--help`

| ID | Command | Expected stdout | Exit code |
|----|---------|-----------------|-----------|
| F-13 | `greet --help` | usage text containing `--name`, `--locale`, `--format` | 0 |
| F-14 | `greet -h` | same as F-13 | 0 |

### 1.5 Unknown flags

| ID | Command | Expected stderr | Exit code |
|----|---------|-----------------|-----------|
| F-15 | `greet --typo` | unknown flag message | 1 |

---

## 2. Locales

The `--locale` flag selects the greeting language.
Default locale when the flag is omitted is `en`.

### 2.1 Supported locales

| ID | Command | Expected greeting prefix | Exit code |
|----|---------|--------------------------|-----------|
| L-01 | `greet --name Alice --locale en` | `Hello, Alice!` | 0 |
| L-02 | `greet --name Alice --locale es` | `¡Hola, Alice!` | 0 |
| L-03 | `greet --name Alice --locale fr` | `Bonjour, Alice !` | 0 |
| L-04 | `greet --name Alice --locale de` | `Hallo, Alice!` | 0 |
| L-05 | `greet --name Alice --locale ja` | `こんにちは、Alice！` | 0 |
| L-06 | `greet --name Alice --locale zh` | `你好，Alice！` | 0 |
| L-07 | `greet --name Alice --locale pt` | `Olá, Alice!` | 0 |
| L-08 | `greet --name Alice --locale ar` | `مرحبا، Alice!` | 0 |

### 2.2 Default locale

| ID | Command | Expected stdout | Exit code |
|----|---------|-----------------|-----------|
| L-09 | `greet --name Alice` (no `--locale`) | `Hello, Alice!` | 0 |

### 2.3 Locale fallback

| ID | Command | Expected behaviour | Exit code |
|----|---------|-------------------|-----------|
| L-10 | `greet --name Alice --locale en-US` | treats `en-US` as `en`; greets in English | 0 |
| L-11 | `greet --name Alice --locale xx` | non-empty error on stderr: unsupported locale | 1 |
| L-12 | `greet --name Alice --locale ""` | non-empty error on stderr | 1 |

### 2.4 Locale + `--loud`

| ID | Command | Expected stdout | Exit code |
|----|---------|-----------------|-----------|
| L-13 | `greet --name Alice --locale es --loud` | uppercased Spanish greeting | 0 |
| L-14 | `greet --name Alice --locale fr --loud` | uppercased French greeting | 0 |

---

## 3. Output Formats

The `--format` flag controls serialisation.
Accepted values: `text` (default), `json`, `yaml`.

### 3.1 Plain text (default)

| ID | Command | Expected stdout | Exit code |
|----|---------|-----------------|-----------|
| O-01 | `greet --name Alice` | `Hello, Alice!` (bare string, trailing newline) | 0 |
| O-02 | `greet --name Alice --format text` | same as O-01 | 0 |

### 3.2 JSON

| ID | Command | Expected stdout (must parse as valid JSON) | Exit code |
|----|---------|-------------------------------------------|-----------|
| O-03 | `greet --name Alice --format json` | `{"greeting":"Hello, Alice!"}` | 0 |
| O-04 | `greet --name Alice --locale es --format json` | `{"greeting":"¡Hola, Alice!"}` | 0 |
| O-05 | `greet --name Alice --loud --format json` | `{"greeting":"HELLO, ALICE!"}` | 0 |
| O-06 | `greet --name Alice --times 3 --format json` | `{"greetings":["Hello, Alice!","Hello, Alice!","Hello, Alice!"]}` | 0 |

JSON validity assertion: parse with a standard JSON parser; any parse error fails the test.

### 3.3 YAML

| ID | Command | Expected stdout (must parse as valid YAML) | Exit code |
|----|---------|-------------------------------------------|-----------|
| O-07 | `greet --name Alice --format yaml` | `greeting: "Hello, Alice!"` | 0 |
| O-08 | `greet --name Alice --locale fr --format yaml` | `greeting: "Bonjour, Alice !"` | 0 |
| O-09 | `greet --name Alice --times 2 --format yaml` | `greetings:` list with two identical entries | 0 |

YAML validity assertion: parse with a standard YAML parser; any parse error fails the test.

### 3.4 Invalid format

| ID | Command | Expected stderr | Exit code |
|----|---------|-----------------|-----------|
| O-10 | `greet --format xml` | non-empty error: unsupported format | 1 |
| O-11 | `greet --format ""` | non-empty error | 1 |

---

## 4. Cross-cutting Scenarios

These cases combine two or more dimensions.

| ID | Command | Expected behaviour | Exit code |
|----|---------|-------------------|-----------|
| X-01 | `greet --name Alice --locale ja --format json` | valid JSON with Japanese greeting | 0 |
| X-02 | `greet --name Alice --locale de --loud --format yaml` | valid YAML with uppercased German greeting | 0 |
| X-03 | `greet --name Alice --times 2 --locale es --format json` | JSON array with two Spanish greetings | 0 |
| X-04 | `greet --name Alice --times 2 --loud --locale fr --format text` | two uppercased French greetings on separate lines | 0 |
| X-05 | `greet --locale xx --format json` | error on stderr; no JSON on stdout | 1 |

---

## 5. Environment & Edge Cases

| ID | Scenario | Expected behaviour | Exit code |
|----|---------|-------------------|-----------|
| E-01 | Name contains Unicode (e.g. `--name 日向`) | greeting preserves name unchanged | 0 |
| E-02 | Name contains shell metacharacters (`--name "O'Brien"`) | greeting preserves name unchanged | 0 |
| E-03 | Very long name (512 chars) | greeting includes full name; no truncation | 0 |
| E-04 | `--times 100` | emits exactly 100 greeting lines | 0 |
| E-05 | `GREET_LOCALE=fr greet --name Alice` (env var, no flag) | greets in French | 0 |
| E-06 | `GREET_LOCALE=fr greet --name Alice --locale de` (flag overrides env) | greets in German | 0 |
| E-07 | Stdout redirected to `/dev/null`; check exit code only | exits 0, no crash | 0 |

---

## 6. Test Execution Notes

- **Binary under test**: build with `make build`; path `./bin/clipse greet` (or a standalone `./bin/greet` if extracted as its own binary).
- **Harness**: any shell-level test runner (e.g. `bats`, `shunit2`, or Go `TestMain` subprocess tests) that can capture stdout/stderr separately and assert on exit code.
- **Locale strings**: assert on the exact byte sequence documented above; update this table if the greeting format changes.
- **JSON/YAML field names**: assert on the schema (`greeting` string for single, `greetings` array for `--times > 1`); field order is not significant for JSON.
- **Exit codes**: all error paths must exit non-zero; the exact code (1 vs. 2) is implementation-defined — tests should assert `!= 0`, not a specific value.
- **Flakiness guard**: none of these tests involve timing, networking, or randomness; every case must be deterministic and repeatable.
