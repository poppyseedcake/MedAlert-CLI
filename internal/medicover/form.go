package medicover

import (
	"html"
	"net/url"
	"strings"
)

// loginForm is a parsed password form. values holds every hidden input plus
// the visible fields; callers override username and password without logging
// any value.
type loginForm struct {
	action string
	values url.Values
}

// mfaForm is a parsed multi-factor form with the same preservation rule.
type mfaForm struct {
	action string
	values url.Values
}

func parseLoginForm(body, pageURL string) (loginForm, error) {
	action, inputs, err := parseFirstForm(body, pageURL)
	if err != nil {
		return loginForm{}, protocolChanged("unexpected login response")
	}
	hasUser, hasPass := false, false
	for name := range inputs {
		lowered := strings.ToLower(name)
		if strings.Contains(lowered, "username") {
			hasUser = true
		}
		if strings.Contains(lowered, "password") {
			hasPass = true
		}
	}
	if !hasUser || !hasPass {
		return loginForm{}, protocolChanged("unexpected login response")
	}
	// The current Medicover form requires the anti-forgery token. A missing
	// token is a protocol change, not an invalid password.
	if !hasAntiForgeryToken(inputs) {
		return loginForm{}, protocolChanged("unexpected login response")
	}
	return loginForm{action: action, values: inputs}, nil
}

func parseMFAForm(body, pageURL string) (mfaForm, error) {
	action, inputs, err := parseFirstForm(body, pageURL)
	if err != nil {
		return mfaForm{}, protocolChanged("unexpected MFA response")
	}
	if !hasMFAField(body) {
		// The caller already checked for an MFA field, but re-check on the
		// parsed inputs to reject pages that only mention MFA in text.
		found := false
		for name := range inputs {
			if strings.Contains(strings.ToLower(name), "mfacode") {
				found = true
				break
			}
		}
		if !found {
			return mfaForm{}, protocolChanged("unexpected MFA response")
		}
	}
	return mfaForm{action: action, values: inputs}, nil
}

func hasAntiForgeryToken(values url.Values) bool {
	for name := range values {
		if name == "__RequestVerificationToken" {
			return true
		}
	}
	return false
}

func (f loginForm) valuesWithCredentials(username, password string) url.Values {
	out := url.Values{}
	for key, list := range f.values {
		out[key] = append([]string(nil), list...)
	}
	setBySubstring(out, "username", username)
	setBySubstring(out, "password", password)
	return out
}

func (f mfaForm) valuesWithCode(code string) url.Values {
	out := url.Values{}
	for key, list := range f.values {
		out[key] = append([]string(nil), list...)
	}
	setBySubstring(out, "mfacode", code)
	// Trusted-device markers from the MediCzuwacz flow. They are only sent
	// when the server presented an MFA form.
	setExact(out, "Input.IsTrustedDevice", "true")
	setExact(out, "Input.DeviceName", "Chrome")
	setExact(out, "Input.Button", "confirm")
	return out
}

func setBySubstring(values url.Values, needle, value string) {
	needle = strings.ToLower(needle)
	for key := range values {
		if strings.Contains(strings.ToLower(key), needle) {
			values.Set(key, value)
			return
		}
	}
	// Fall back to the current Medicover field names when the form uses an
	// unexpected casing. The hidden-field preservation above keeps any
	// unknown fields untouched.
	if needle == "username" {
		values.Set("Input.Username", value)
	} else if needle == "password" {
		values.Set("Input.Password", value)
	} else if needle == "mfacode" {
		values.Set("Input.MfaCode", value)
	}
}

func setExact(values url.Values, key, value string) {
	// Preserve the server's exact key when a case variant already exists.
	for existing := range values {
		if strings.EqualFold(existing, key) {
			values.Set(existing, value)
			return
		}
	}
	values.Set(key, value)
}

// parseFirstForm returns the first HTML form action (resolved against
// pageURL) and all of its input values. Unknown hidden fields are preserved
// verbatim; their values are never logged.
func parseFirstForm(body, pageURL string) (string, url.Values, error) {
	formTag, formBody := firstFormSection(body)
	if formTag == "" {
		return "", nil, protocolChanged("missing login form")
	}
	attributes := parseAttributes(formTag)
	action := strings.TrimSpace(attributes["action"])
	if action == "" {
		action = pageURL
	} else {
		action = resolveURL(pageURL, html.UnescapeString(action))
	}
	values := url.Values{}
	for _, inputTag := range findInputTags(formBody) {
		attrs := parseAttributes(inputTag)
		name, ok := lookupAttr(attrs, "name")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		name = html.UnescapeString(strings.TrimSpace(name))
		value := ""
		if raw, ok := lookupAttr(attrs, "value"); ok {
			value = html.UnescapeString(raw)
		}
		// Checkboxes and radios only contribute when checked.
		if inputType, ok := lookupAttr(attrs, "type"); ok {
			lowered := strings.ToLower(strings.TrimSpace(inputType))
			if (lowered == "checkbox" || lowered == "radio") && !hasAttr(attrs, "checked") {
				continue
			}
		}
		values.Add(name, value)
	}
	if len(values) == 0 {
		return "", nil, protocolChanged("empty login form")
	}
	return action, values, nil
}

func firstFormSection(body string) (formTag, formBody string) {
	lowered := strings.ToLower(body)
	start := strings.Index(lowered, "<form")
	if start < 0 {
		return "", ""
	}
	tagEnd := strings.Index(body[start:], ">")
	if tagEnd < 0 {
		return "", ""
	}
	tagEnd += start
	formTag = body[start : tagEnd+1]
	rest := body[tagEnd+1:]
	end := strings.Index(strings.ToLower(rest), "</form>")
	if end < 0 {
		formBody = rest
	} else {
		formBody = rest[:end]
	}
	return formTag, formBody
}

func findInputTags(formBody string) []string {
	var tags []string
	lowered := strings.ToLower(formBody)
	cursor := 0
	for {
		index := strings.Index(lowered[cursor:], "<input")
		if index < 0 {
			return tags
		}
		index += cursor
		end := strings.Index(formBody[index:], ">")
		if end < 0 {
			return tags
		}
		end += index
		tags = append(tags, formBody[index:end+1])
		cursor = end + 1
		if cursor >= len(formBody) {
			return tags
		}
	}
}

// parseAttributes parses one HTML tag into lower-cased attribute names.
func parseAttributes(tag string) map[string]string {
	attributes := map[string]string{}
	// Strip the leading "<form"/"<input" and trailing ">" or "/>".
	inner := strings.TrimSpace(tag)
	if index := strings.Index(inner, " "); index >= 0 {
		inner = inner[index+1:]
	} else {
		return attributes
	}
	inner = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(inner), ">"))
	inner = strings.TrimSpace(strings.TrimSuffix(inner, "/"))
	cursor := 0
	for cursor < len(inner) {
		for cursor < len(inner) && isAttrSpace(inner[cursor]) {
			cursor++
		}
		if cursor >= len(inner) {
			break
		}
		nameStart := cursor
		for cursor < len(inner) && !isAttrSpace(inner[cursor]) && inner[cursor] != '=' && inner[cursor] != '>' && inner[cursor] != '/' {
			cursor++
		}
		name := strings.ToLower(strings.TrimSpace(inner[nameStart:cursor]))
		if name == "" || name == "/" {
			cursor++
			continue
		}
		for cursor < len(inner) && isAttrSpace(inner[cursor]) {
			cursor++
		}
		if cursor >= len(inner) || inner[cursor] != '=' {
			attributes[name] = ""
			continue
		}
		cursor++
		for cursor < len(inner) && isAttrSpace(inner[cursor]) {
			cursor++
		}
		if cursor >= len(inner) {
			attributes[name] = ""
			break
		}
		quote := inner[cursor]
		if quote == '"' || quote == '\'' {
			cursor++
			valueStart := cursor
			end := strings.IndexByte(inner[valueStart:], quote)
			if end < 0 {
				attributes[name] = inner[valueStart:]
				break
			}
			attributes[name] = inner[valueStart : valueStart+end]
			cursor = valueStart + end + 1
		} else {
			valueStart := cursor
			for cursor < len(inner) && !isAttrSpace(inner[cursor]) && inner[cursor] != '>' {
				cursor++
			}
			attributes[name] = inner[valueStart:cursor]
		}
	}
	return attributes
}

func isAttrSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func lookupAttr(attrs map[string]string, name string) (string, bool) {
	value, ok := attrs[strings.ToLower(name)]
	return value, ok
}

func hasAttr(attrs map[string]string, name string) bool {
	_, ok := attrs[strings.ToLower(name)]
	return ok
}

func resolveURL(base, ref string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return baseURL.ResolveReference(refURL).String()
}

func hasPasswordField(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "type=\"password\"") ||
		strings.Contains(lowered, "type='password'") ||
		strings.Contains(lowered, "type=password") ||
		strings.Contains(lowered, "input.password")
}

func hasMFAField(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "mfacode")
}
