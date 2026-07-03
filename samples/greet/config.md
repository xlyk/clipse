# greet — configuration schema

The `greet` tool is configured via a YAML block under the `greet` key of `clipse.yaml`.

```yaml
greet:
  template: "Hello, {name}!"
  default_name: "World"
  locale: "en"
```

---

## Fields

### `template`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"Hello, {name}!"` |
| **Required** | no |

A Go-style format string used to render the greeting. The placeholder `{name}` is replaced at runtime with the resolved name (see `default_name`).

**Examples**

```yaml
template: "Hello, {name}!"          # English default
template: "Hola, {name}!"           # Spanish
template: "¡Buenos días, {name}!"   # locale-aware variant
```

---

### `default_name`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"World"` |
| **Required** | no |

The name substituted into `{name}` when no explicit name is supplied at invocation time. Callers that always pass a name explicitly may omit this field.

**Examples**

```yaml
default_name: "World"     # produces "Hello, World!"
default_name: "Alice"     # produces "Hello, Alice!"
```

---

### `locale`

| | |
|---|---|
| **Type** | `string` (BCP 47 language tag) |
| **Default** | `"en"` |
| **Required** | no |

Controls locale-sensitive formatting (punctuation, character set, direction). Must be a valid [BCP 47](https://www.rfc-editor.org/rfc/rfc5646) language tag.

**Accepted values (non-exhaustive)**

| Tag | Language |
|-----|----------|
| `en` | English |
| `es` | Spanish |
| `fr` | French |
| `de` | German |
| `ja` | Japanese |
| `zh` | Chinese |

**Examples**

```yaml
locale: "en"      # English
locale: "es"      # Spanish
locale: "fr-CA"   # Canadian French
```

---

## Minimal configuration

Only the `greet` key is required; all sub-fields have defaults:

```yaml
greet: {}
```

This is equivalent to:

```yaml
greet:
  template: "Hello, {name}!"
  default_name: "World"
  locale: "en"
```

---

## Full example

```yaml
greet:
  template: "¡Hola, {name}!"
  default_name: "Mundo"
  locale: "es"
```

Output: `¡Hola, Mundo!`
