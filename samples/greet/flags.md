# `greet` CLI flags

This document specifies the flags accepted by the `greet` command.

## `--name`

| Property | Value |
|----------|-------|
| Type | `string` |
| Default | `"World"` |
| Required | No |

The name of the person (or thing) to greet.

**Example**

```
greet --name Alice
# Hello, Alice!
```

---

## `--locale`

| Property | Value |
|----------|-------|
| Type | `string` |
| Default | `"en"` |
| Required | No |

BCP 47 language tag that controls the greeting phrase.

Supported values:

| Tag | Greeting |
|-----|----------|
| `en` | Hello |
| `es` | Hola |
| `fr` | Bonjour |
| `de` | Hallo |
| `ja` | こんにちは |

Unrecognised tags fall back to `en`.

**Example**

```
greet --name María --locale es
# Hola, María!
```

---

## `--loud`

| Property | Value |
|----------|-------|
| Type | `bool` |
| Default | `false` |
| Required | No |

When set, the greeting is printed in ALL CAPS.

**Example**

```
greet --name Bob --loud
# HELLO, BOB!
```

---

## Combined example

```
greet --name Claude --locale fr --loud
# BONJOUR, CLAUDE!
```
