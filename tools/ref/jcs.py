"""RFC 8785 JSON Canonicalization Scheme (JCS), for the value types the manifest uses.

https://www.rfc-editor.org/rfc/rfc8785 :
- §3.2.1 no whitespace between tokens;
- §3.2.2.2 strings: escape only '"', '\\' and U+0000..U+001F, using \\b \\t \\n \\f \\r and otherwise
  \\u00xx with lowercase hex; every other character is written as is (UTF-8, §3.2.4);
- §3.2.2.3 numbers: ECMAScript serialization. The manifest has only integers (sizes, counts), whose
  ECMAScript form for |n| <= 2^53-1 is the plain decimal; floats are rejected rather than implemented;
- §3.2.3 object members sorted by the UTF-16 code units of their names.
"""

MAX_SAFE_INT = 2**53 - 1
_ESCAPES = {'"': '\\"', "\\": "\\\\", "\b": "\\b", "\t": "\\t", "\n": "\\n", "\f": "\\f", "\r": "\\r"}


def _string(s):
    out = []
    for ch in s:
        if ch in _ESCAPES:
            out.append(_ESCAPES[ch])
        elif ord(ch) < 0x20:
            out.append("\\u%04x" % ord(ch))
        else:
            out.append(ch)
    return '"' + "".join(out) + '"'


def _value(v):
    if v is None:
        return "null"
    if v is True:
        return "true"
    if v is False:
        return "false"
    if isinstance(v, int):
        if abs(v) > MAX_SAFE_INT:
            raise ValueError("integer outside the IEEE 754 safe range: %d" % v)
        return str(v)
    if isinstance(v, float):
        raise ValueError("floats are not used by the manifest and not supported by this JCS subset")
    if isinstance(v, str):
        return _string(v)
    if isinstance(v, list):
        return "[" + ",".join(_value(x) for x in v) + "]"
    if isinstance(v, dict):
        keys = sorted(v, key=lambda k: k.encode("utf-16-be"))
        return "{" + ",".join(_string(k) + ":" + _value(v[k]) for k in keys) + "}"
    raise TypeError("not a JSON value: %r" % (v,))


def canonicalize(value):
    """Return the JCS canonical UTF-8 bytes of a parsed JSON value."""
    return _value(value).encode("utf-8")
