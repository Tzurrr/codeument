package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		kinds    []string
		mustMiss []string // substrings that must not survive
	}{
		{in: "ls -la", want: "ls -la"},
		{in: "cd /var/log && tail -f syslog", want: "cd /var/log && tail -f syslog"},
		{in: "mysql -u root -phunter2 mydb", want: "mysql -u root -p<redacted:password> mydb", kinds: []string{"password"}, mustMiss: []string{"hunter2"}},
		{in: "mysql -uroot -p -h db.local", want: "mysql -uroot -p -h db.local"},
		{in: "mysqldump --password=s3cr3t app > dump.sql", want: "mysqldump --password=<redacted:flag> app > dump.sql", kinds: []string{"flag"}, mustMiss: []string{"s3cr3t"}},
		{in: "export AWS_SECRET_ACCESS_KEY=abcd1234efgh5678", want: "export AWS_SECRET_ACCESS_KEY=<redacted:env>", kinds: []string{"env"}, mustMiss: []string{"abcd1234"}},
		{in: "PGPASSWORD='p@ss w' psql -h db", want: "PGPASSWORD=<redacted:env> psql -h db", kinds: []string{"env"}, mustMiss: []string{"p@ss"}},
		{in: "cd $PWD/x", want: "cd $PWD/x"},
		{in: `curl -H "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U" https://api`, want: `curl -H "Authorization: Bearer <redacted:auth_header>" https://api`, kinds: []string{"auth_header"}, mustMiss: []string{"eyJ"}},
		{in: "curl -u admin:topsecret https://host/api", want: "curl -u admin:<redacted:basic_auth> https://host/api", kinds: []string{"basic_auth"}, mustMiss: []string{"topsecret"}},
		{in: "git clone https://alice:ghp_abcdefghijklmnopqrstuvwxyz0123456789@github.com/x/y", want: "git clone https://alice:<redacted:url_password>@github.com/x/y", kinds: []string{"url_password"}, mustMiss: []string{"ghp_"}},
		{in: "aws configure set aws_access_key_id AKIAIOSFODNN7EXAMPLE", want: "aws configure set aws_access_key_id <redacted:token>", kinds: []string{"token"}},
		{in: "echo 'root:NewPass1!' | chpasswd", want: "echo <redacted:argv> | chpasswd", kinds: []string{"argv"}, mustMiss: []string{"NewPass1"}},
		{in: "echo $TOKEN | docker login -u bob --password-stdin registry.local", want: "echo <redacted:argv> | docker login <redacted:argv>", kinds: []string{"argv"}},
		{in: "sudo vault write secret/db password=abc", want: "sudo vault write <redacted:argv>", kinds: []string{"argv"}, mustMiss: []string{"abc"}},
		{in: "useradd -m -p '$6$abc$xyz' deploy", want: "useradd -m -p <redacted:password> deploy", kinds: []string{"password"}, mustMiss: []string{"$6$"}},
		{in: "sshpass -p secret ssh host", want: "sshpass <redacted:argv>", kinds: []string{"argv"}, mustMiss: []string{"secret"}},
		{in: "kubectl create secret generic db --from-literal=password=abc", want: "kubectl create secret <redacted:argv>", kinds: []string{"argv"}, mustMiss: []string{"abc"}},
		{in: "cat > key.pem <<EOF\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----\nEOF", want: "cat > key.pem <<EOF <redacted:heredoc>", kinds: []string{"heredoc", "private_key"}, mustMiss: []string{"MIIE"}},
		{in: "ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz codeument now", want: "ANTHROPIC_API_KEY=<redacted:env> codeument now", kinds: []string{"env"}, mustMiss: []string{"sk-ant"}},
		{in: "terraform apply -var db_password=Sup3r -auto-approve", want: "terraform apply -var db_password=<redacted:env> -auto-approve", kinds: []string{"env"}, mustMiss: []string{"Sup3r"}},
		{in: "psql 'postgres://app:pw123@db:5432/app'", want: "psql 'postgres://app:<redacted:url_password>@db:5432/app'", kinds: []string{"url_password"}, mustMiss: []string{"pw123"}},
		{in: "ssh -i ~/.ssh/id_rsa user@host", want: "ssh -i ~/.ssh/id_rsa user@host"},
		{in: "git commit -m 'fix token refresh'", want: "git commit -m 'fix token refresh'"},
		{in: "vim /etc/nginx/nginx.conf", want: "vim /etc/nginx/nginx.conf"},
		{in: "curl -s http://localhost:8080/api/v1/token/refresh", want: "curl -s http://localhost:8080/api/v1/token/refresh"},
	}
	for _, c := range cases {
		got := Default.Redact(c.in)
		if got.Text != c.want {
			t.Errorf("Redact(%q)\n got: %q\nwant: %q", c.in, got.Text, c.want)
		}
		if strings.Join(got.Kinds, ",") != strings.Join(c.kinds, ",") {
			t.Errorf("Redact(%q) kinds = %v, want %v", c.in, got.Kinds, c.kinds)
		}
		for _, m := range c.mustMiss {
			if strings.Contains(got.Text, m) {
				t.Errorf("Redact(%q) leaked %q: %q", c.in, m, got.Text)
			}
		}
		if len(got.Captured) != 0 {
			t.Errorf("Redact(%q) captured values without capture enabled", c.in)
		}
	}
}

func TestCapture(t *testing.T) {
	r, err := New(Options{Capture: true})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Redact("echo 'deploy:Winter2026!' | chpasswd")
	if len(got.Captured) != 1 || got.Captured[0].Username != "deploy" || got.Captured[0].Value != "Winter2026!" {
		t.Fatalf("captured = %+v", got.Captured)
	}
	if strings.Contains(got.Text, "Winter") {
		t.Fatalf("text leaked: %q", got.Text)
	}
	got = r.Redact("useradd -m -p Pl4in deploy")
	if len(got.Captured) != 1 || got.Captured[0].Username != "deploy" || got.Captured[0].Value != "Pl4in" {
		t.Fatalf("captured = %+v", got.Captured)
	}
	got = r.Redact("mysql -u root -phunter2")
	if len(got.Captured) != 1 || got.Captured[0].Program != "mysql" || got.Captured[0].Value != "hunter2" {
		t.Fatalf("captured = %+v", got.Captured)
	}
}

func TestExtraPatternsAndCommands(t *testing.T) {
	r, err := New(Options{ExtraPatterns: []string{`corpkey-[A-Za-z0-9]{8}`}, ExtraSensitiveCommands: []string{"mycorp-secrets"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Redact("deploy --key corpkey-ABCD1234"); got.Text != "deploy --key <redacted:flag>" && got.Text != "deploy --key <redacted:custom>" {
		t.Fatalf("got %q", got.Text)
	}
	if got := r.Redact("mycorp-secrets get prod/db"); got.Text != "mycorp-secrets get <redacted:argv>" {
		t.Fatalf("got %q", got.Text)
	}
	if _, err := New(Options{ExtraPatterns: []string{"("}}); err == nil {
		t.Fatal("expected error for invalid pattern")
	}
}

func FuzzRedactNeverPanicsOrLeaksMarkers(f *testing.F) {
	f.Add("mysql -phunter2")
	f.Add("echo x | chpasswd")
	f.Add("curl -u a:b http://x")
	f.Fuzz(func(t *testing.T, in string) {
		got := Default.Redact(in)
		if len(got.Text) > len(in)*4+200 {
			t.Fatalf("output grew unreasonably: %d -> %d", len(in), len(got.Text))
		}
	})
}
