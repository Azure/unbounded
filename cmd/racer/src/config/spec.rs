//! Declarative environment table engine.
//!
//! Every environment variable the process accepts is one [`Var`]. Parsing,
//! defaults, per-variable bounds, the unknown-variable check, and the
//! generated reference documentation all come from the same entries, so a
//! setting cannot be documented, defaulted, and parsed in three places that
//! drift apart. Cross-variable rules stay in code and report every variable
//! they involve.

use std::collections::BTreeMap;
use std::fmt;

/// Prefix of every variable this process owns. Any other variable carrying it
/// is rejected as unknown, which catches typos and stale names.
pub const PREFIX: &str = "RACER_";

/// Longest accepted value, in bytes, for any variable.
pub const MAX_VALUE_BYTES: usize = 4096;

/// One accepted environment variable.
#[derive(Clone, Copy, Debug)]
pub struct Var {
    pub name: &'static str,
    pub kind: Kind,
    pub default: Default,
    pub scope: Scope,
    pub owner: Owner,
    /// Reference documentation section.
    pub group: Group,
    pub doc: &'static str,
}

/// Value syntax and single-variable bounds.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    /// ASCII decimal digits only, within `min..=max` and a multiple of `step`
    /// (`step` 1 accepts every value).
    Unsigned {
        min: u64,
        max: u64,
        step: u64,
        unit: Unit,
    },
    /// Exactly `true` or `false`.
    Bool,
    /// Exactly one of the listed words.
    Enum(&'static [&'static str]),
    /// Free text; deeper checks live with the consumer.
    Text(TextKind),
}

/// What a text value names. Used for documentation and for the checks the
/// consumer applies after the table's text rules.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TextKind {
    Path,
    Address,
    Url,
    Uuid,
    Ip,
    IpList,
    Name,
    Image,
    Other,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Unit {
    None,
    Bytes,
    Millis,
    Seconds,
    Percent,
    BytesPerSecond,
}

/// Value used when a variable is unset.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Default {
    /// Parsed with the variable's own rules, so it is always in bounds.
    Value(&'static str),
    /// No value; the setting is absent or derived as described.
    Unset(&'static str),
}

/// How a quantity relates to the node's I/O workers.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Scope {
    /// One node-wide value. Budgets that workers share are divided among them.
    Node,
    /// Applies to each I/O worker. Shared budgets are multiplied by the final
    /// I/O worker count before they are divided back out.
    PerWorker,
}

/// Who reads a variable.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Owner {
    /// The dataplane process.
    Dataplane,
    /// Embedded cluster control, in managed deployments.
    Authority,
    /// The operator. The dataplane accepts and ignores it, because the
    /// operator's settings ConfigMap reaches the Pod environment whole.
    Operator,
}

/// Documentation sections, in output order.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Group {
    Control,
    Threads,
    Hardware,
    Gpu,
    Storage,
    Admission,
    Limits,
    Network,
    Origin,
    Prefetch,
    Timeouts,
    Diagnostics,
    Kubernetes,
    Operator,
}

impl Group {
    pub fn title(self) -> &'static str {
        match self {
            Self::Control => "Control and identity",
            Self::Threads => "Threads",
            Self::Hardware => "RDMA and CPU",
            Self::Gpu => "GPU reads",
            Self::Storage => "Storage",
            Self::Admission => "Disk retention",
            Self::Limits => "Admission budgets",
            Self::Network => "Listeners and peers",
            Self::Origin => "Origins",
            Self::Prefetch => "Prefetch",
            Self::Timeouts => "Timeouts",
            Self::Diagnostics => "Diagnostics",
            Self::Kubernetes => "Cluster control (managed mode)",
            Self::Operator => "Operator",
        }
    }
}

/// A configuration failure that names the variable, its value, and why it
/// was refused.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ConfigError {
    /// The offending variable, or every variable a cross-variable rule involves.
    pub name: String,
    /// The offending value, when there is exactly one.
    pub value: Option<String>,
    pub reason: String,
}

impl ConfigError {
    pub fn new(name: impl Into<String>, reason: impl Into<String>) -> Self {
        Self {
            name: name.into(),
            value: None,
            reason: reason.into(),
        }
    }

    pub fn value(name: impl Into<String>, value: &str, reason: impl Into<String>) -> Self {
        Self {
            name: name.into(),
            value: Some(value.to_owned()),
            reason: reason.into(),
        }
    }
}

impl fmt::Display for ConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match &self.value {
            Some(value) => {
                // Bound the echo so a pasted secret or huge value stays short.
                let shown: String = value.chars().take(128).collect();
                let more = if shown.len() < value.len() { "..." } else { "" };
                write!(f, "{}={:?}{more}: {}", self.name, shown, self.reason)
            }
            None => write!(f, "{}: {}", self.name, self.reason),
        }
    }
}

impl std::error::Error for ConfigError {}

/// A parsed, table-checked value.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Value {
    Unsigned(u64),
    Bool(bool),
    Text(String),
}

/// Every table variable after syntax and single-variable bound checks.
pub struct Values {
    table: &'static [Var],
    /// Present for set variables and for unset ones with a default.
    values: BTreeMap<&'static str, (Value, bool)>,
}

impl Values {
    /// Check each table variable that `lookup` returns. Empty values are
    /// rejected rather than treated as unset, except where `empty_unset`
    /// names the variable (Kubernetes expands some field references to "").
    pub fn load(
        table: &'static [Var],
        mut lookup: impl FnMut(&str) -> Result<Option<String>, ConfigError>,
        empty_unset: &dyn Fn(&str) -> bool,
    ) -> Result<Self, ConfigError> {
        let mut values = BTreeMap::new();
        for var in table {
            let raw = lookup(var.name)?.filter(|v| !(v.is_empty() && empty_unset(var.name)));
            match raw {
                Some(raw) => {
                    values.insert(var.name, (parse(var, &raw)?, true));
                }
                None => {
                    if let Default::Value(text) = var.default {
                        values.insert(var.name, (parse(var, text)?, false));
                    }
                }
            }
        }
        Ok(Self { table, values })
    }

    fn var(&self, name: &str) -> &'static Var {
        self.table
            .iter()
            .find(|var| var.name == name)
            .unwrap_or_else(|| panic!("{name} is not in the configuration table"))
    }

    /// Whether the environment set `name` explicitly.
    pub fn is_set(&self, name: &str) -> bool {
        self.var(name);
        self.values.get(name).is_some_and(|(_, set)| *set)
    }

    pub fn unsigned(&self, name: &str) -> Option<u64> {
        self.var(name);
        match self.values.get(name) {
            Some((Value::Unsigned(value), _)) => Some(*value),
            Some(_) => panic!("{name} is not unsigned"),
            None => None,
        }
    }

    pub fn boolean(&self, name: &str) -> Option<bool> {
        self.var(name);
        match self.values.get(name) {
            Some((Value::Bool(value), _)) => Some(*value),
            Some(_) => panic!("{name} is not a boolean"),
            None => None,
        }
    }

    pub fn text(&self, name: &str) -> Option<&str> {
        self.var(name);
        match self.values.get(name) {
            Some((Value::Text(value), _)) => Some(value),
            Some(_) => panic!("{name} is not text"),
            None => None,
        }
    }

    /// Raw text of a set or defaulted variable, for error messages.
    pub fn shown(&self, name: &str) -> Option<String> {
        self.values.get(name).map(|(value, _)| match value {
            Value::Unsigned(n) => n.to_string(),
            Value::Bool(b) => b.to_string(),
            Value::Text(t) => t.clone(),
        })
    }
}

/// Reject variables with the owned prefix that the table does not list.
pub fn reject_unknown<'a>(
    table: &[Var],
    names: impl IntoIterator<Item = &'a str>,
) -> Result<(), ConfigError> {
    for name in names {
        if name.starts_with(PREFIX) && !table.iter().any(|var| var.name == name) {
            return Err(ConfigError::new(
                name,
                "unknown variable; see the Racer configuration reference for accepted names",
            ));
        }
    }
    Ok(())
}

/// Apply a variable's syntax and single-variable bounds to one value.
pub fn parse(var: &Var, raw: &str) -> Result<Value, ConfigError> {
    let invalid = |reason: String| ConfigError::value(var.name, raw, reason);
    if raw.is_empty() {
        return Err(invalid("must not be empty".into()));
    }
    if raw.len() > MAX_VALUE_BYTES {
        return Err(invalid(format!("longer than {MAX_VALUE_BYTES} bytes")));
    }
    if raw.chars().any(char::is_control) {
        return Err(invalid("contains a control character".into()));
    }
    match var.kind {
        Kind::Unsigned {
            min,
            max,
            step,
            unit: _,
        } => {
            let value = unsigned(raw).ok_or_else(|| {
                invalid("must be an unsigned decimal integer (ASCII digits only)".into())
            })?;
            if value < min || value > max {
                return Err(invalid(format!("must be from {min} to {max}")));
            }
            if step > 1 && !value.is_multiple_of(step) {
                return Err(invalid(format!("must be a multiple of {step}")));
            }
            Ok(Value::Unsigned(value))
        }
        Kind::Bool => match raw {
            "true" => Ok(Value::Bool(true)),
            "false" => Ok(Value::Bool(false)),
            _ => Err(invalid("must be exactly true or false".into())),
        },
        Kind::Enum(words) => {
            if words.contains(&raw) {
                Ok(Value::Text(raw.to_owned()))
            } else {
                Err(invalid(format!("must be one of {}", words.join(", "))))
            }
        }
        Kind::Text(_) => Ok(Value::Text(raw.to_owned())),
    }
}

/// Accept only ASCII decimal digits within the full u64 range. Rust's
/// `FromStr` alone would also accept a leading `+`.
pub fn unsigned(value: &str) -> Option<u64> {
    if value.is_empty() || !value.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    value.parse().ok()
}

fn markdown_cell(text: &str) -> String {
    text.replace('|', "\\|")
}

fn grouped(value: u64) -> String {
    let digits = value.to_string();
    let mut out = String::new();
    for (index, c) in digits.chars().enumerate() {
        if index > 0 && (digits.len() - index).is_multiple_of(3) {
            out.push(',');
        }
        out.push(c);
    }
    out
}

fn bytes_note(value: u64) -> Option<String> {
    const UNITS: [(u64, &str); 4] = [
        (1 << 40, "TiB"),
        (1 << 30, "GiB"),
        (1 << 20, "MiB"),
        (1 << 10, "KiB"),
    ];
    UNITS
        .iter()
        .find(|(size, _)| value >= *size && value.is_multiple_of(*size))
        .map(|(size, unit)| format!("{} {unit}", value / size))
}

/// Accepted values, as shown in the reference table.
pub fn accepted(var: &Var) -> String {
    match var.kind {
        Kind::Unsigned {
            min,
            max,
            step,
            unit,
        } => {
            let unit = match unit {
                Unit::None => "",
                Unit::Bytes => " bytes",
                Unit::Millis => " ms",
                Unit::Seconds => " s",
                Unit::Percent => " %",
                Unit::BytesPerSecond => " bytes/s",
            };
            let max = if max == u64::MAX {
                "2^64-1".to_owned()
            } else {
                grouped(max)
            };
            let step = if step > 1 {
                format!(", multiple of {}", grouped(step))
            } else {
                String::new()
            };
            format!("{}..{max}{unit}{step}", grouped(min))
        }
        Kind::Bool => "`true`, `false`".into(),
        Kind::Enum(words) => words
            .iter()
            .map(|w| format!("`{w}`"))
            .collect::<Vec<_>>()
            .join(", "),
        Kind::Text(kind) => match kind {
            TextKind::Path => "absolute path",
            TextKind::Address => "`ip:port`",
            TextKind::Url => "`https://` URL",
            TextKind::Uuid => "UUID",
            TextKind::Ip => "IP address",
            TextKind::IpList => "comma-separated IP addresses",
            TextKind::Name => "DNS label",
            TextKind::Image => "image reference",
            TextKind::Other => "text",
        }
        .into(),
    }
}

/// Default value cell for the reference table.
pub fn default_cell(var: &Var) -> String {
    match var.default {
        Default::Value(text) => {
            let note = match var.kind {
                Kind::Unsigned {
                    unit: Unit::Bytes, ..
                } => unsigned(text).and_then(bytes_note),
                _ => None,
            };
            match note {
                Some(note) => format!("`{text}` ({note})"),
                None => format!("`{text}`"),
            }
        }
        Default::Unset("") => "Unset".into(),
        Default::Unset(note) => format!("Unset: {note}"),
    }
}

/// The generated reference: one Markdown table per group, in group order.
pub fn markdown(table: &[Var]) -> String {
    let mut groups: Vec<Group> = table.iter().map(|var| var.group).collect();
    groups.sort();
    groups.dedup();
    let mut out = String::new();
    for group in groups {
        out.push_str(&format!("#### {}\n\n", group.title()));
        out.push_str("| Variable | Default | Scope | Accepted | Description |\n");
        out.push_str("| --- | --- | --- | --- | --- |\n");
        for var in table.iter().filter(|var| var.group == group) {
            let scope = match (var.owner, var.scope) {
                (Owner::Operator, _) => "Operator",
                (Owner::Authority, _) => "Node",
                (_, Scope::Node) => "Node",
                (_, Scope::PerWorker) => "Per I/O worker",
            };
            out.push_str(&format!(
                "| `{}` | {} | {} | {} | {} |\n",
                var.name,
                markdown_cell(&default_cell(var)),
                scope,
                markdown_cell(&accepted(var)),
                markdown_cell(var.doc),
            ));
        }
        out.push('\n');
    }
    out
}

fn json_string(text: &str) -> String {
    let mut out = String::from("\"");
    for c in text.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// Machine-readable table for out-of-process validators such as the operator.
pub fn json(table: &[Var], empty_unset: &dyn Fn(&str) -> bool) -> String {
    let mut out = String::from("[\n");
    for (index, var) in table.iter().enumerate() {
        let owner = match var.owner {
            Owner::Dataplane => "dataplane",
            Owner::Authority => "authority",
            Owner::Operator => "operator",
        };
        let scope = match var.scope {
            Scope::Node => "node",
            Scope::PerWorker => "per-worker",
        };
        let kind = match var.kind {
            Kind::Unsigned { min, max, step, .. } => format!(
                "\"kind\": \"unsigned\", \"min\": {}, \"max\": {}, \"step\": {}",
                json_string(&min.to_string()),
                json_string(&max.to_string()),
                json_string(&step.to_string())
            ),
            Kind::Bool => "\"kind\": \"bool\"".into(),
            Kind::Enum(words) => format!(
                "\"kind\": \"enum\", \"values\": [{}]",
                words
                    .iter()
                    .map(|w| json_string(w))
                    .collect::<Vec<_>>()
                    .join(", ")
            ),
            Kind::Text(_) => "\"kind\": \"text\"".into(),
        };
        let default = match var.default {
            Default::Value(text) => json_string(text),
            Default::Unset(_) => "null".into(),
        };
        out.push_str(&format!(
            "  {{\"name\": {}, \"owner\": \"{owner}\", \"scope\": \"{scope}\", {kind}, \"default\": {default}, \"empty_unset\": {}}}",
            json_string(var.name),
            empty_unset(var.name)
        ));
        out.push_str(if index + 1 == table.len() {
            "\n"
        } else {
            ",\n"
        });
    }
    out.push_str("]\n");
    out
}

impl From<ConfigError> for crate::error::Error {
    /// The named detail is logged where the error is produced; callers that
    /// only propagate it see the generic configuration failure.
    fn from(_: ConfigError) -> Self {
        Self::InvalidConfiguration
    }
}
