package main

import (
	"strings"
	"testing"
)

func TestAuthPassword(t *testing.T) {
	a := authPassword("s3cret")
	if !a.required() {
		t.Fatal("password mode must require auth")
	}
	if _, err := a.check([]string{"wrong"}); err == nil {
		t.Error("wrong password accepted")
	}
	if _, err := a.check([]string{"s3cret", "extra"}); err == nil {
		t.Error("wrong arity accepted")
	}
	role, err := a.check([]string{"s3cret"})
	if err != nil || role != roleAdmin {
		t.Errorf("want admin, got %v %v", role, err)
	}
}

func TestAuthFile(t *testing.T) {
	path := writeTemp(t, "users.txt", strings.Join([]string{
		"# comment",
		"",
		"app:appsecret",
		"viewer:viewsecret:user",
		"ops:opssecret:admin",
	}, "\n"))
	a, err := authFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		role role
		ok   bool
	}{
		{[]string{"app", "appsecret"}, roleUser, true},
		{[]string{"viewer", "viewsecret"}, roleUser, true},
		{[]string{"ops", "opssecret"}, roleAdmin, true},
		{[]string{"ops", "wrong"}, 0, false},
		{[]string{"ghost", "opssecret"}, 0, false},
		{[]string{"ops"}, 0, false},
	} {
		role, err := a.check(tc.args)
		if tc.ok != (err == nil) || (tc.ok && role != tc.role) {
			t.Errorf("check(%v): got %v %v, want ok=%v role=%v", tc.args, role, err, tc.ok, tc.role)
		}
	}
}

func TestAuthFileRejectsBadInput(t *testing.T) {
	for name, content := range map[string]string{
		"bad role":       "a:b:root",
		"missing pass":   "a:",
		"missing colon":  "abc",
		"duplicate user": "a:b\na:c",
		"empty file":     "# only comments\n",
	} {
		if _, err := authFileStore(writeTemp(t, "u.txt", content)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAuthNone(t *testing.T) {
	a := authNone()
	if a.required() {
		t.Fatal("no-auth mode must not require auth")
	}
	if role, err := a.check(nil); err != nil || role != roleAdmin {
		t.Errorf("auth in no-auth mode should no-op to admin, got %v %v", role, err)
	}
}
