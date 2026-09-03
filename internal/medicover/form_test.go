package medicover

import (
	"strings"
	"testing"
)

func TestParseLoginFormKeepsSelectAndButton(t *testing.T) {
	body := `<html><body><form method="post" action="/Account/Login">` +
		`<input type="hidden" name="__RequestVerificationToken" value="t123" />` +
		`<input type="text" name="Input.Username" /><input type="password" name="Input.Password" />` +
		`<select name="Input.LoginType"><option value="Password">Password</option><option value="Other" selected>Other</option></select>` +
		`<button type="submit" name="Input.Button" value="login">Log in</button>` +
		`</form></body></html>`
	form, err := parseLoginForm(body, "https://login.example/Account/Login")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	posted := form.valuesWithCredentials("user", "pass")
	if posted.Get("Input.LoginType") != "Other" {
		t.Fatalf("LoginType = %q, want Other", posted.Get("Input.LoginType"))
	}
	if posted.Get("Input.Button") != "login" {
		t.Fatalf("Button = %q, want login", posted.Get("Input.Button"))
	}
	if !strings.Contains(posted.Encode(), "Input.Username") {
		t.Fatal("username missing")
	}
}
