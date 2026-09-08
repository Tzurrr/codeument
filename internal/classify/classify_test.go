package classify

import (
	"reflect"
	"testing"

	"github.com/Tzurrr/codeument/internal/model"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		cmd       string
		kind      model.Kind
		family    string
		weight    int
		milestone bool
		files     []string
	}{
		{"ls -la", model.KindNoise, "noise", 0, false, nil},
		{"cd /etc && ls", model.KindNoise, "noise", 0, false, nil},
		{"cat /etc/hosts", model.KindNoise, "noise", 0, false, nil},
		{"grep -r foo .", model.KindNoise, "noise", 0, false, nil},
		{"git status", model.KindMeaningful, "git", 1, false, nil},
		{"git commit -m 'x'", model.KindMeaningful, "git", 5, true, nil},
		{"git push origin main", model.KindMeaningful, "git", 5, true, nil},
		{"sudo systemctl restart nginx", model.KindMeaningful, "service", 5, true, nil},
		{"sudo apt-get install -y nginx", model.KindMeaningful, "package", 4, false, nil},
		{"systemctl status nginx", model.KindMeaningful, "service", 1, false, nil},
		{"docker compose up -d", model.KindMeaningful, "docker", 5, true, nil},
		{"docker ps", model.KindMeaningful, "docker", 1, false, nil},
		{"kubectl get pods -n prod", model.KindMeaningful, "kubernetes", 1, false, nil},
		{"kubectl apply -f deploy.yaml", model.KindMeaningful, "kubernetes", 5, true, nil},
		{"terraform apply -auto-approve", model.KindMeaningful, "terraform", 5, true, nil},
		{"vim /etc/nginx/nginx.conf", model.KindMeaningful, "config", 3, false, []string{"/etc/nginx/nginx.conf"}},
		{"nano docker-compose.yml", model.KindMeaningful, "config", 3, false, []string{"/srv/app/docker-compose.yml"}},
		{"vim main.go", model.KindMeaningful, "edit", 2, false, []string{"/srv/app/main.go"}},
		{"echo 'x' > /etc/motd", model.KindMeaningful, "fs", 2, false, []string{"/etc/motd"}},
		{"make build && ./bin/app", model.KindMeaningful, "build", 2, false, nil},
		{"ssh web-01", model.KindMeaningful, "ssh", 2, false, nil},
		{"crontab -e", model.KindMeaningful, "cron", 3, false, []string{"crontab"}},
		{"sudo chown -R app:app /srv/app", model.KindMeaningful, "fs", 4, false, []string{"/srv/app"}},
		{"cat foo | grep bar | sort", model.KindNoise, "noise", 0, false, nil},
		{"ENV=prod ./deploy.sh", model.KindMeaningful, "other", 1, false, nil},
		{"tail -f /var/log/nginx/error.log", model.KindNoise, "noise", 0, false, nil},
	}
	for _, c := range cases {
		got := Default.Classify(c.cmd, "/srv/app")
		if got.Kind != c.kind || got.Family != c.family || got.Weight != c.weight || got.Milestone != c.milestone {
			t.Errorf("Classify(%q) = {%s %s %d %v}, want {%s %s %d %v}", c.cmd, got.Kind, got.Family, got.Weight, got.Milestone, c.kind, c.family, c.weight, c.milestone)
		}
		if !reflect.DeepEqual(got.FilesTouched, c.files) {
			t.Errorf("Classify(%q) files = %v, want %v", c.cmd, got.FilesTouched, c.files)
		}
	}
}

func TestOptions(t *testing.T) {
	c := New(Options{Ignore: []string{"myscript"}, Unignore: []string{"cat"}, Weights: map[string]int{"kubectl get": 4, "deploy-prod": 6}})
	if got := c.Classify("myscript --run", ""); got.Kind != model.KindNoise {
		t.Errorf("ignore not applied: %+v", got)
	}
	if got := c.Classify("cat /etc/passwd", ""); got.Kind != model.KindMeaningful || got.Weight != 1 {
		t.Errorf("unignore not applied: %+v", got)
	}
	if got := c.Classify("kubectl get pods", ""); got.Weight != 4 {
		t.Errorf("weight override not applied: %+v", got)
	}
	if got := c.Classify("deploy-prod v1.2", ""); !got.Milestone || got.Weight != 6 {
		t.Errorf("custom milestone not applied: %+v", got)
	}
}
