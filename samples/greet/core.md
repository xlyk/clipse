# Greeter core

The greeter produces a single greeting string by combining two inputs: a
**config** that describes the audience and presentation, and a **message
catalog** that holds the localised text.

## Inputs

### Config

The greeter reads a small config block (typically from `clipse.yaml` or a
dedicated `greet.yaml`) that controls _how_ a greeting is assembled:

```yaml
greet:
  # locale selects the message set from the catalog.
  locale: "en"

  # style picks one message variant within the locale set.
  # Allowed values: formal | casual | emoji
  style: "casual"

  # name is the recipient; interpolated into the message template.
  name: "World"
```

| Field    | Type   | Default    | Purpose                                      |
|----------|--------|------------|----------------------------------------------|
| `locale` | string | `"en"`     | Selects a top-level key in the catalog        |
| `style`  | string | `"casual"` | Selects a variant within the locale set       |
| `name`   | string | `"World"`  | The `{{.Name}}` placeholder value             |

### Message catalog

The catalog is a YAML file (e.g. `samples/greet/catalog.yaml`) that maps every
`(locale, style)` pair to a Go template string:

```yaml
en:
  formal: "Good day, {{.Name}}."
  casual: "Hey, {{.Name}}!"
  emoji:  "👋 {{.Name}}!"
es:
  formal: "Buenos días, {{.Name}}."
  casual: "¡Hola, {{.Name}}!"
  emoji:  "👋 ¡{{.Name}}!"
```

The only required placeholder is `{{.Name}}`; catalog entries that omit it
produce a valid (name-less) greeting rather than an error.

## Combination logic

```
greeting = catalog[config.locale][config.style]
         | catalog[config.locale]["casual"]        // style fallback
         | catalog["en"]["casual"]                  // locale fallback
```

Steps in order:

1. **Locale lookup** — find the locale key in the catalog.  If absent, fall
   back to `"en"`.
2. **Style lookup** — within that locale set, find the style key.  If absent,
   fall back to `"casual"`.
3. **Template execution** — render the selected template string with
   `{Name: config.name}` as the data object.
4. **Return** — the rendered string is the final greeting.

No greeting is ever empty: the `"en"/"casual"` entry in the catalog is the
guaranteed baseline, and the catalog loader rejects a catalog that omits it.

## Example

Config:

```yaml
greet:
  locale: "es"
  style:  "formal"
  name:   "Ada"
```

Catalog entry resolved: `"Buenos días, {{.Name}}."` → **`Buenos días, Ada.`**

## Error conditions

| Condition                             | Behaviour                              |
|---------------------------------------|----------------------------------------|
| Catalog file missing or unreadable    | Hard error — greeter refuses to start  |
| `en.casual` entry absent from catalog | Hard error — invariant violation       |
| Requested locale absent               | Silent fallback to `en`                |
| Requested style absent                | Silent fallback to `casual`            |
| Template execution error              | Hard error with the offending template |
