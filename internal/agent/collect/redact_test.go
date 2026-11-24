// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// tokenFixture is a cloudflared connector token: base64 JSON whose "s" is the
// tunnel's secret.
const tokenFixture = "eyJhIjoiMjc0ZmFhMWQxMTFhZjcxNmEzMzkyMmIzNGVlNTM4MWQiLCJ0IjoiMzI3ZDlkNDEtYjAwOS00MTNkLTg0OTQtN2VlZWVmYjNlZDJhIiwicyI6InNlY3JldCJ9"

func TestRedactHidesSecretFlags(t *testing.T) {
	t.Parallel()
	const sec = "S3cretVal"
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		// Cloudflared connector tokens and install.
		{"cloudflared --token", []string{"cloudflared", "tunnel", "run", "--token", tokenFixture}, []string{"cloudflared", "tunnel", "run", "--token", Hidden}},
		{"cloudflared --token=", []string{"cloudflared", "tunnel", "run", "--token=" + tokenFixture}, []string{"cloudflared", "tunnel", "run", "--token=" + Hidden}},
		{"cloudflared -token", []string{"cloudflared", "-token", tokenFixture, "tunnel", "run"}, []string{"cloudflared", "-token", Hidden, "tunnel", "run"}},
		{"cloudflared credentials-contents", []string{"cloudflared", "tunnel", "run", "--credentials-contents", `{"TunnelSecret":"x"}`}, []string{"cloudflared", "tunnel", "run", "--credentials-contents", Hidden}},
		{"cloudflared sh -c", []string{"sh", "-c", "exec cloudflared tunnel run --token " + tokenFixture + " --loglevel info"}, []string{"sh", "-c", "exec cloudflared tunnel run --token " + Hidden + " --loglevel info"}},
		{"cloudflared --token-file kept", []string{"cloudflared", "tunnel", "run", "--token-file", "/run/secrets/tunnel"}, []string{"cloudflared", "tunnel", "run", "--token-file", "/run/secrets/tunnel"}},
		{"cloudflared truncated token", []string{"cloudflared", "tunnel", "run", "--token", tokenFixture[:20]}, []string{"cloudflared", "tunnel", "run", "--token", Hidden}},
		{"cloudflared sh -c -token", []string{"sh", "-c", "cloudflared -token " + tokenFixture + " tunnel run"}, []string{"sh", "-c", "cloudflared -token " + Hidden + " tunnel run"}},
		{"cloudflared service install", []string{"/usr/bin/cloudflared", "service", "install", tokenFixture}, []string{"/usr/bin/cloudflared", "service", "install", Hidden}},
		{"sudo cloudflared service install", []string{"sudo", "cloudflared", "service", "install", tokenFixture}, []string{"sudo", "cloudflared", "service", "install", Hidden}},
		{"sudo sh -c cloudflared service install", []string{"sudo", "sh", "-c", "cloudflared service install " + tokenFixture}, []string{"sudo", "sh", "-c", "cloudflared service install " + Hidden}},
		{"cloudflared positional tunnel name kept", []string{"cloudflared", "tunnel", "run", "my-prod-tunnel"}, []string{"cloudflared", "tunnel", "run", "my-prod-tunnel"}},

		// (1) Per-program short password flags (glued and separate, direct and in sh -c / wrappers).
		{"mysql -p glued", []string{"mysql", "-uroot", "-p" + sec, "appdb"}, []string{"mysql", "-uroot", "-p" + Hidden, "appdb"}},
		{"mysql -p separate", []string{"mysql", "-uroot", "-p", sec, "appdb"}, []string{"mysql", "-uroot", "-p", Hidden, "appdb"}},
		{"/usr/bin/mysql -p glued", []string{"/usr/bin/mysql", "-h", "db", "-p" + sec}, []string{"/usr/bin/mysql", "-h", "db", "-p" + Hidden}},
		{"mysqladmin -p separate", []string{"mysqladmin", "ping", "-h", "127.0.0.1", "-p", sec}, []string{"mysqladmin", "ping", "-h", "127.0.0.1", "-p", Hidden}},
		{"mysqladmin -p glued", []string{"mysqladmin", "-uroot", "-p" + sec, "ping"}, []string{"mysqladmin", "-uroot", "-p" + Hidden, "ping"}},
		{"mariadb -p glued", []string{"mariadb", "-u", "admin", "-p" + sec}, []string{"mariadb", "-u", "admin", "-p" + Hidden}},
		{"mysqldump -p glued", []string{"mysqldump", "-uroot", "-p" + sec, "mydb"}, []string{"mysqldump", "-uroot", "-p" + Hidden, "mydb"}},
		{"sudo mysql -p glued", []string{"sudo", "mysql", "-p" + sec}, []string{"sudo", "mysql", "-p" + Hidden}},
		{"sh -c mysqladmin healthcheck", []string{"sh", "-c", "mysqladmin ping -h localhost -p" + sec}, []string{"sh", "-c", "mysqladmin ping -h localhost -p" + Hidden}},
		{"sh -c mysql quoted password with spaces", []string{"sh", "-c", `mysql -uroot -p"my ` + sec + ` pass" db`}, []string{"sh", "-c", "mysql -uroot -p" + Hidden + " db"}},
		{"redis-cli -a separate", []string{"redis-cli", "-h", "127.0.0.1", "-a", sec, "ping"}, []string{"redis-cli", "-h", "127.0.0.1", "-a", Hidden, "ping"}},
		{"redis-cli -a glued", []string{"redis-cli", "-a" + sec, "INFO"}, []string{"redis-cli", "-a" + Hidden, "INFO"}},
		{"sh -c redis-cli -a", []string{"sh", "-c", "redis-cli -a " + sec + " ping"}, []string{"sh", "-c", "redis-cli -a " + Hidden + " ping"}},
		{"sshpass -p separate", []string{"sshpass", "-p", sec, "ssh", "user@host"}, []string{"sshpass", "-p", Hidden, "ssh", "user@host"}},
		{"sshpass -p glued", []string{"sshpass", "-p" + sec, "scp", "a", "user@host:b"}, []string{"sshpass", "-p" + Hidden, "scp", "a", "user@host:b"}},
		{"openssl enc -k separate", []string{"openssl", "enc", "-aes-256-cbc", "-k", sec, "-in", "a"}, []string{"openssl", "enc", "-aes-256-cbc", "-k", Hidden, "-in", "a"}},
		{"openssl enc -k glued", []string{"openssl", "enc", "-aes-256-cbc", "-k" + sec}, []string{"openssl", "enc", "-aes-256-cbc", "-k" + Hidden}},
		{"openssl -passin pass:", []string{"openssl", "rsa", "-in", "k.pem", "-passin", "pass:" + sec}, []string{"openssl", "rsa", "-in", "k.pem", "-passin", "pass:" + Hidden}},
		{"openssl -passout pass:", []string{"openssl", "pkcs12", "-export", "-passout", "pass:" + sec}, []string{"openssl", "pkcs12", "-export", "-passout", "pass:" + Hidden}},
		{"openssl -pass pass:", []string{"openssl", "enc", "-aes-256-cbc", "-pass", "pass:" + sec}, []string{"openssl", "enc", "-aes-256-cbc", "-pass", "pass:" + Hidden}},
		{"openssl -passin file: kept", []string{"openssl", "rsa", "-in", "k.pem", "-passin", "file:/run/secrets/pw"}, []string{"openssl", "rsa", "-in", "k.pem", "-passin", "file:/run/secrets/pw"}},
		{"openssl -passin env: kept", []string{"openssl", "rsa", "-in", "k.pem", "-passin", "env:KEY_PASS"}, []string{"openssl", "rsa", "-in", "k.pem", "-passin", "env:KEY_PASS"}},
		{"sh -c pipeline resets program", []string{"sh", "-c", "mysql -p" + sec + " db | ssh -p 2222 host"}, []string{"sh", "-c", "mysql -p" + Hidden + " db | ssh -p 2222 host"}},

		// (2) User and password in one argument (curl -u / --user).
		{"curl -u user:pass", []string{"curl", "-s", "-u", "admin:" + sec, "https://api.example.com"}, []string{"curl", "-s", "-u", "admin:" + Hidden, "https://api.example.com"}},
		{"curl -u glued", []string{"curl", "-uadmin:" + sec, "https://api.example.com"}, []string{"curl", "-uadmin:" + Hidden, "https://api.example.com"}},
		{"curl --user separate", []string{"curl", "--user", "admin:" + sec, "https://api.example.com"}, []string{"curl", "--user", "admin:" + Hidden, "https://api.example.com"}},
		{"curl --user=", []string{"curl", "--user=admin:" + sec, "https://api.example.com"}, []string{"curl", "--user=admin:" + Hidden, "https://api.example.com"}},
		{"sh -c curl -u", []string{"sh", "-c", "curl -u admin:" + sec + " https://api.example.com"}, []string{"sh", "-c", "curl -u admin:" + Hidden + " https://api.example.com"}},
		{"curl -u user without password kept", []string{"curl", "-u", "admin", "https://api.example.com"}, []string{"curl", "-u", "admin", "https://api.example.com"}},

		// (3) Header values (Authorization, Bearer, Basic, X-Api-Key).
		{"curl -H Authorization Bearer", []string{"curl", "-H", "Authorization: Bearer " + sec, "https://api.example.com"}, []string{"curl", "-H", "Authorization: Bearer " + Hidden, "https://api.example.com"}},
		{"curl -H Authorization Basic", []string{"curl", "-H", "Authorization: Basic dXNlcjpwYXNz", "https://api.example.com"}, []string{"curl", "-H", "Authorization: Basic " + Hidden, "https://api.example.com"}},
		{"curl -H Authorization custom scheme", []string{"curl", "-H", "Authorization: Token " + sec, "https://api.example.com"}, []string{"curl", "-H", "Authorization: " + Hidden, "https://api.example.com"}},
		{"curl -H Proxy-Authorization", []string{"curl", "-H", "Proxy-Authorization: Basic " + sec, "https://api.example.com"}, []string{"curl", "-H", "Proxy-Authorization: Basic " + Hidden, "https://api.example.com"}},
		{"curl -H X-Api-Key", []string{"curl", "-H", "X-Api-Key: " + sec, "https://api.example.com"}, []string{"curl", "-H", "X-Api-Key: " + Hidden, "https://api.example.com"}},
		{"sh -c curl -H Authorization Bearer", []string{"sh", "-c", `curl -H "Authorization: Bearer ` + sec + `" https://api.example.com`}, []string{"sh", "-c", `curl -H "Authorization: Bearer ` + Hidden + `" https://api.example.com`}},
		{"sh -c curl -H Authorization Basic", []string{"sh", "-c", `curl -H 'Authorization: Basic ` + sec + `' https://api.example.com`}, []string{"sh", "-c", `curl -H 'Authorization: Basic ` + Hidden + `' https://api.example.com`}},
		{"standalone Bearer value", []string{"app", "--header", "Bearer " + sec}, []string{"app", "--header", "Bearer " + Hidden}},
		{"standalone Basic value", []string{"app", "--header", "Basic " + sec}, []string{"app", "--header", "Basic " + Hidden}},

		// (4) URL credentials and query-string secrets.
		{"URL user:pass", []string{"curl", "https://user:" + sec + "@api.example.com/v1"}, []string{"curl", "https://user:" + Hidden + "@api.example.com/v1"}},
		{"URL query token and api_key", []string{"curl", "https://h/x?token=" + sec + "&api_key=K123&page=2"}, []string{"curl", "https://h/x?token=" + Hidden + "&api_key=" + Hidden + "&page=2"}},
		{"postgres URL user:pass and query password", []string{"psql", "postgres://u:" + sec + "@h/db?password=Q99&sslmode=require"}, []string{"psql", "postgres://u:" + Hidden + "@h/db?password=" + Hidden + "&sslmode=require"}},
		{"URL query secret with fragment", []string{"curl", "https://h/x?access_token=" + sec + "#frag"}, []string{"curl", "https://h/x?access_token=" + Hidden + "#frag"}},
		{"URL query non-secret kept", []string{"curl", "https://h/x?page=2&sort=asc"}, []string{"curl", "https://h/x?page=2&sort=asc"}},

		// (5) Java system properties (-D...password=...).
		{"java -Dspring.datasource.password", []string{"java", "-Dspring.datasource.password=" + sec, "-jar", "app.jar"}, []string{"java", "-Dspring.datasource.password=" + Hidden, "-jar", "app.jar"}},
		{"java -Djavax.net.ssl.keyStorePassword", []string{"java", "-Djavax.net.ssl.keyStorePassword=" + sec, "-jar", "app.jar"}, []string{"java", "-Djavax.net.ssl.keyStorePassword=" + Hidden, "-jar", "app.jar"}},
		{"java -Dapp.apiKey", []string{"java", "-Dapp.apiKey=" + sec, "-jar", "app.jar"}, []string{"java", "-Dapp.apiKey=" + Hidden, "-jar", "app.jar"}},
		{"java -Dauth.token", []string{"java", "-Dauth.token=" + sec, "-jar", "app.jar"}, []string{"java", "-Dauth.token=" + Hidden, "-jar", "app.jar"}},
		{"java -Dserver.port kept", []string{"java", "-Dserver.port=8080", "-Djavax.net.ssl.keyStore=/etc/ks.jks", "-jar", "app.jar"}, []string{"java", "-Dserver.port=8080", "-Djavax.net.ssl.keyStore=/etc/ks.jks", "-jar", "app.jar"}},

		// (6) Colon separator on long secret flags.
		{"--password:value", []string{"app", "--password:" + sec}, []string{"app", "--password:" + Hidden}},
		{"--api-key:value", []string{"app", "--api-key:" + sec}, []string{"app", "--api-key:" + Hidden}},
		{"--token:value", []string{"app", "--token:" + sec}, []string{"app", "--token:" + Hidden}},

		// (7) Widened flags and env/KEY=value names.
		{"--password and --port", []string{"app", "--password", sec, "--port", "8080"}, []string{"app", "--password", Hidden, "--port", "8080"}},
		{"--db-password=", []string{"app", "--db-password=" + sec}, []string{"app", "--db-password=" + Hidden}},
		{"--pw", []string{"app", "--pw", sec}, []string{"app", "--pw", Hidden}},
		{"--admin-pw=", []string{"app", "--admin-pw=" + sec}, []string{"app", "--admin-pw=" + Hidden}},
		{"tailscale --authkey=", []string{"tailscale", "up", "--authkey=tskey-auth-" + sec}, []string{"tailscale", "up", "--authkey=" + Hidden}},
		{"tailscale --auth-key", []string{"tailscale", "up", "--auth-key", "tskey-" + sec}, []string{"tailscale", "up", "--auth-key", Hidden}},
		{"--secret_key_base", []string{"rails", "s", "--secret_key_base=" + sec}, []string{"rails", "s", "--secret_key_base=" + Hidden}},
		{"redis-server --requirepass", []string{"redis-server", "--requirepass", sec}, []string{"redis-server", "--requirepass", Hidden}},
		{"curl --api-key --client-secret=", []string{"curl", "--api-key", sec, "--client-secret=" + sec}, []string{"curl", "--api-key", Hidden, "--client-secret=" + Hidden}},
		{"--password-file kept", []string{"app", "--password-file", "/run/secrets/pw"}, []string{"app", "--password-file", "/run/secrets/pw"}},
		{"PGPASSWORD and PORT", []string{"env", "PGPASSWORD=" + sec, "PORT=5432", "psql"}, []string{"env", "PGPASSWORD=" + Hidden, "PORT=5432", "psql"}},
		{"API_KEY kept SORT_KEY PASSWORD_FILE", []string{"env", "API_KEY=" + sec, "SORT_KEY=name", "PASSWORD_FILE=/etc/pw", "app"}, []string{"env", "API_KEY=" + Hidden, "SORT_KEY=name", "PASSWORD_FILE=/etc/pw", "app"}},
		{"DATABASE_URL=", []string{"env", "DATABASE_URL=postgres://admin:" + sec + "@db:5432/app", "app"}, []string{"env", "DATABASE_URL=" + Hidden, "app"}},
		{"GITHUB_PAT=", []string{"env", "GITHUB_PAT=ghp_" + sec, "gh"}, []string{"env", "GITHUB_PAT=" + Hidden, "gh"}},
		{"STRIPE_KEY=", []string{"env", "STRIPE_KEY=sk_live_" + sec, "app"}, []string{"env", "STRIPE_KEY=" + Hidden, "app"}},
		{"AUTH=", []string{"env", "AUTH=" + sec, "app"}, []string{"env", "AUTH=" + Hidden, "app"}},
		{"JWT_SIGNING=", []string{"env", "JWT_SIGNING=" + sec, "app"}, []string{"env", "JWT_SIGNING=" + Hidden, "app"}},
		{"SLACK_WEBHOOK=", []string{"env", "SLACK_WEBHOOK=https://hooks.slack.com/services/T0/B0/" + sec, "app"}, []string{"env", "SLACK_WEBHOOK=" + Hidden, "app"}},
		{"SECRET_KEY_BASE=", []string{"env", "SECRET_KEY_BASE=" + sec, "rails"}, []string{"env", "SECRET_KEY_BASE=" + Hidden, "rails"}},
		{"AUTH_MODE and SIGNING_ALG kept", []string{"env", "AUTH_MODE=oidc", "SIGNING_ALG=RS256", "WEBHOOK_ENABLED=true", "app"}, []string{"env", "AUTH_MODE=oidc", "SIGNING_ALG=RS256", "WEBHOOK_ENABLED=true", "app"}},

		// (8) JSON object arguments.
		{"JSON password arg", []string{"app", "--config", `{"password":"` + sec + `"}`}, []string{"app", "--config", Hidden}},
		{"JSON token arg", []string{"app", "--config", `{"api_token":"` + sec + `","host":"db"}`}, []string{"app", "--config", Hidden}},
		{"JSON secret glued =", []string{"app", `--config={"secret":"` + sec + `"}`}, []string{"app", "--config=" + Hidden}},
		{"JSON in sh -c", []string{"sh", "-c", `app --config '{"password":"` + sec + `"}'`}, []string{"sh", "-c", `app --config '` + Hidden + `'`}},
		{"JSON benign kept", []string{"app", "--config", `{"host":"127.0.0.1","port":8080}`}, []string{"app", "--config", `{"host":"127.0.0.1","port":8080}`}},

		// (9) Quoted values with spaces inside sh -c (including truncated unclosed quotes).
		{"sh -c KEY double-quoted with space", []string{"sh", "-c", `TOKEN="` + sec + ` tail" app`}, []string{"sh", "-c", "TOKEN=" + Hidden + " app"}},
		{"sh -c KEY single-quoted with space", []string{"sh", "-c", `API_KEY='` + sec + ` extra words' app`}, []string{"sh", "-c", "API_KEY=" + Hidden + " app"}},
		{"sh -c --token= double-quoted with space", []string{"sh", "-c", `app --token="a ` + sec + ` b" --port 80`}, []string{"sh", "-c", "app --token=" + Hidden + " --port 80"}},
		{"sh -c --password= single-quoted with space", []string{"sh", "-c", `app --password='two words ` + sec + `' --verbose`}, []string{"sh", "-c", "app --password=" + Hidden + " --verbose"}},
		{"sh -c truncated unclosed quote", []string{"sh", "-c", `TOKEN="` + sec + ` truncated tail`}, []string{"sh", "-c", "TOKEN=" + Hidden}},
		{"sh -c PGPASSWORD and --password=", []string{"sh", "-c", "PGPASSWORD=" + sec + " psql --password=" + sec + " -h db"}, []string{"sh", "-c", "PGPASSWORD=" + Hidden + " psql --password=" + Hidden + " -h db"}},

		// (10) Token as URL userinfo with no password.
		{"https bare GitHub PAT userinfo", []string{"git", "clone", "https://ghp_" + sec + "@github.com/org/repo.git"}, []string{"git", "clone", "https://" + Hidden + "@github.com/org/repo.git"}},
		{"https bare generic token userinfo", []string{"curl", "https://mytoken123@api.example.com/v1"}, []string{"curl", "https://" + Hidden + "@api.example.com/v1"}},
		{"http bare token userinfo", []string{"curl", "http://tok_" + sec + "@internal/api"}, []string{"curl", "http://" + Hidden + "@internal/api"}},
		{"ssh user@host kept", []string{"ssh", "ssh://root@example.com:22"}, []string{"ssh", "ssh://root@example.com:22"}},

		// (11) Round 3: wrappers with arguments (sudo -u, timeout, nice -n, env, docker exec).
		{"sudo -u root mysql -p", []string{"sudo", "-u", "root", "mysql", "-p" + sec}, []string{"sudo", "-u", "root", "mysql", "-p" + Hidden}},
		{"timeout 5 redis-cli -a", []string{"timeout", "5", "redis-cli", "-a", sec}, []string{"timeout", "5", "redis-cli", "-a", Hidden}},
		{"nice -n 10 mysqldump -p", []string{"nice", "-n", "10", "mysqldump", "-p" + sec}, []string{"nice", "-n", "10", "mysqldump", "-p" + Hidden}},
		{"docker exec db mysql -p", []string{"docker", "exec", "db", "mysql", "-uroot", "-p" + sec}, []string{"docker", "exec", "db", "mysql", "-uroot", "-p" + Hidden}},
		{"docker exec -it -u root db mysql -p", []string{"docker", "exec", "-it", "-u", "root", "db", "mysql", "-uroot", "-p" + sec}, []string{"docker", "exec", "-it", "-u", "root", "db", "mysql", "-uroot", "-p" + Hidden}},
		{"env -i mysql -p", []string{"env", "-i", "FOO=bar", "mysql", "-p" + sec}, []string{"env", "-i", "FOO=bar", "mysql", "-p" + Hidden}},
		{"sh -c timeout mysqladmin -p", []string{"sh", "-c", "timeout 5 mysqladmin ping -p" + sec}, []string{"sh", "-c", "timeout 5 mysqladmin ping -p" + Hidden}},
		{"sudo -u mysql ssh -p 22 kept", []string{"sudo", "-u", "mysql", "ssh", "-p", "22", "host"}, []string{"sudo", "-u", "mysql", "ssh", "-p", "22", "host"}},

		// (12) Round 3: shell structure inside sh -c (glued ;, &&, ||, |, (), $(), ``, if/then).
		{"sh -c glued semicolon", []string{"sh", "-c", "a=1;mysql -p" + sec}, []string{"sh", "-c", "a=1;mysql -p" + Hidden}},
		{"sh -c glued &&", []string{"sh", "-c", "x&&mysql -p" + sec}, []string{"sh", "-c", "x&&mysql -p" + Hidden}},
		{"sh -c glued ||", []string{"sh", "-c", "x||mysql -p" + sec}, []string{"sh", "-c", "x||mysql -p" + Hidden}},
		{"sh -c glued |", []string{"sh", "-c", "x|redis-cli -a " + sec}, []string{"sh", "-c", "x|redis-cli -a " + Hidden}},
		{"sh -c subshell parens", []string{"sh", "-c", "(mysql -p" + sec + ")"}, []string{"sh", "-c", "(mysql -p" + Hidden + ")"}},
		{"sh -c command substitution $()", []string{"sh", "-c", "$(mysql -p" + sec + ")"}, []string{"sh", "-c", "$(mysql -p" + Hidden + ")"}},
		{"sh -c backticks", []string{"sh", "-c", "`mysql -p" + sec + "`"}, []string{"sh", "-c", "`mysql -p" + Hidden + "`"}},
		{"sh -c if then fi", []string{"sh", "-c", "if true; then mysql -p" + sec + "; fi"}, []string{"sh", "-c", "if true; then mysql -p" + Hidden + "; fi"}},

		// (13) Round 3: dotted long flags and --env=/--set=KEY=VALUE.
		{"spring dotted password flag", []string{"java", "-jar", "app.jar", "--spring.datasource.password=" + sec}, []string{"java", "-jar", "app.jar", "--spring.datasource.password=" + Hidden}},
		{"db dotted password flag", []string{"app", "--db.password=" + sec}, []string{"app", "--db.password=" + Hidden}},
		{"helm --set= nested password", []string{"helm", "upgrade", "rel", "chart", "--set=global.redis.password=" + sec}, []string{"helm", "upgrade", "rel", "chart", "--set=global.redis.password=" + Hidden}},
		{"docker run --env=POSTGRES_PASSWORD=", []string{"docker", "run", "--env=POSTGRES_PASSWORD=" + sec, "postgres"}, []string{"docker", "run", "--env=POSTGRES_PASSWORD=" + Hidden, "postgres"}},
		{"docker run -p 8080:80 kept", []string{"docker", "run", "-p", "8080:80", "nginx"}, []string{"docker", "run", "-p", "8080:80", "nginx"}},

		// (14) Round 3: additional credential headers and curl -d/--data bodies.
		{"curl -H X-Auth-Token", []string{"curl", "-H", "X-Auth-Token: " + sec, "https://api.example.com"}, []string{"curl", "-H", "X-Auth-Token: " + Hidden, "https://api.example.com"}},
		{"curl -H PRIVATE-TOKEN", []string{"curl", "-H", "PRIVATE-TOKEN: " + sec, "https://gitlab.example.com/api/v4/projects"}, []string{"curl", "-H", "PRIVATE-TOKEN: " + Hidden, "https://gitlab.example.com/api/v4/projects"}},
		{"curl -H Cookie", []string{"curl", "-H", "Cookie: session=" + sec, "https://api.example.com"}, []string{"curl", "-H", "Cookie: " + Hidden, "https://api.example.com"}},
		{"curl -H apikey", []string{"curl", "-H", "apikey: " + sec, "https://api.example.com"}, []string{"curl", "-H", "apikey: " + Hidden, "https://api.example.com"}},
		{"curl -H Content-Type kept", []string{"curl", "-H", "Content-Type: application/json", "https://api.example.com"}, []string{"curl", "-H", "Content-Type: application/json", "https://api.example.com"}},
		{"curl -d form password", []string{"curl", "-d", "user=x&password=" + sec, "https://api.example.com/login"}, []string{"curl", "-d", "user=x&password=" + Hidden, "https://api.example.com/login"}},
		{"curl --data client_secret", []string{"curl", "--data", "client_secret=" + sec + "&grant_type=client_credentials", "https://api.example.com/oauth"}, []string{"curl", "--data", "client_secret=" + Hidden + "&grant_type=client_credentials", "https://api.example.com/oauth"}},
		{"curl --data-raw with spaces in secret", []string{"curl", "--data-raw", "user=x&password=my " + sec + "&ok=1", "https://api.example.com"}, []string{"curl", "--data-raw", "user=x&password=" + Hidden + "&ok=1", "https://api.example.com"}},

		// (15) Round 3: more per-program password flags and smaller patterns.
		{"mongosh -p separate", []string{"mongosh", "-u", "admin", "-p", sec}, []string{"mongosh", "-u", "admin", "-p", Hidden}},
		{"mongo -p glued", []string{"mongo", "-u", "admin", "-p" + sec}, []string{"mongo", "-u", "admin", "-p" + Hidden}},
		{"docker login -p separate", []string{"docker", "login", "-u", "me", "-p", sec, "ghcr.io"}, []string{"docker", "login", "-u", "me", "-p", Hidden, "ghcr.io"}},
		{"docker login -p glued", []string{"docker", "login", "-u", "me", "-p" + sec}, []string{"docker", "login", "-u", "me", "-p" + Hidden}},
		{"ldapsearch -w separate", []string{"ldapsearch", "-D", "cn=admin", "-w", sec}, []string{"ldapsearch", "-D", "cn=admin", "-w", Hidden}},
		{"ldapsearch -w glued", []string{"ldapsearch", "-w" + sec}, []string{"ldapsearch", "-w" + Hidden}},
		{"smbclient -U user%pass", []string{"smbclient", "//srv/share", "-U", "user%" + sec}, []string{"smbclient", "//srv/share", "-U", "user%" + Hidden}},
		{"smbclient -U glued", []string{"smbclient", "//srv/share", "-Uuser%" + sec}, []string{"smbclient", "//srv/share", "-Uuser%" + Hidden}},
		{"htpasswd -b f u pass", []string{"htpasswd", "-b", "/etc/htpasswd", "alice", sec}, []string{"htpasswd", "-b", "/etc/htpasswd", "alice", Hidden}},
		{"htpasswd -nb u pass", []string{"htpasswd", "-nb", "alice", sec}, []string{"htpasswd", "-nb", "alice", Hidden}},
		{"vault login token", []string{"vault", "login", sec}, []string{"vault", "login", Hidden}},
		{"MYSQL_PWD env", []string{"env", "MYSQL_PWD=" + sec, "mysql"}, []string{"env", "MYSQL_PWD=" + Hidden, "mysql"}},
		{"mount -o comma password", []string{"mount", "-t", "cifs", "-o", "username=u,password=" + sec, "//h/s", "/mnt"}, []string{"mount", "-t", "cifs", "-o", "username=u,password=" + Hidden, "//h/s", "/mnt"}},
		{"jdbc sqlserver semicolon password", []string{"app", "jdbc:sqlserver://h;password=" + sec}, []string{"app", "jdbc:sqlserver://h;password=" + Hidden}},
		{"Azure SAS sig query", []string{"curl", "https://acct.blob.core.windows.net/c/b?sv=2022&sig=" + sec}, []string{"curl", "https://acct.blob.core.windows.net/c/b?sv=2022&sig=" + Hidden}},
		{"JSON array secret", []string{"app", "--config", `[{"token":"` + sec + `"}]`}, []string{"app", "--config", Hidden}},
		{"sh -c heredoc password line", []string{"sh", "-c", "cat <<EOF\nuser: admin\npassword: " + sec + "\nEOF"}, []string{"sh", "-c", "cat <<EOF\nuser: admin\npassword: " + Hidden + "\nEOF"}},
		{"python3 -c inline password", []string{"python3", "-c", "connect(host='db', password='" + sec + "')"}, []string{"python3", "-c", "connect(host='db', password=" + Hidden + ")"}},
		{"Slack webhook URL path", []string{"curl", "https://hooks.slack.com/services/T00/B00/" + sec}, []string{"curl", "https://hooks.slack.com/services/" + Hidden}},
		{"Discord webhook URL path", []string{"curl", "https://discord.com/api/webhooks/12345/" + sec}, []string{"curl", "https://discord.com/api/webhooks/" + Hidden}},
		{"Telegram bot URL path", []string{"curl", "https://api.telegram.org/bot12345:" + sec + "/sendMessage"}, []string{"curl", "https://api.telegram.org/bot" + Hidden + "/sendMessage"}},

		// Benign commands must not be over-redacted.
		{"--no-token kept", []string{"sh", "-c", "app --no-token x"}, []string{"sh", "-c", "app --no-token x"}},
		{"nginx -g kept", []string{"nginx", "-g", "daemon off;"}, []string{"nginx", "-g", "daemon off;"}},
		{"ssh -p 2222 kept", []string{"ssh", "-p", "2222", "root@example.com"}, []string{"ssh", "-p", "2222", "root@example.com"}},
		{"mkdir -p kept", []string{"mkdir", "-p", "/var/lib/dockwell"}, []string{"mkdir", "-p", "/var/lib/dockwell"}},
		{"cp -a kept", []string{"cp", "-a", "/src", "/dst"}, []string{"cp", "-a", "/src", "/dst"}},
		{"ps -u root kept", []string{"ps", "-u", "root"}, []string{"ps", "-u", "root"}},
	}

	if len(cases) < 85 {
		t.Fatalf("expected at least 85 redaction test cases, got %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redact(c.in)
			if !slices.Equal(got, c.want) {
				t.Errorf("redact(%q)\n got  %q\n want %q", c.in, got, c.want)
			}
			// Double-check no case that passed sec leaked it.
			if strings.Contains(strings.Join(c.in, " "), sec) && strings.Contains(strings.Join(got, " "), sec) {
				t.Errorf("secret %q leaked in output %q", sec, got)
			}
		})
	}
}

// The process list the agent sends never holds a connector token, as a unit that
// "cloudflared service install" wrote runs one on its command line (ADR 0104, the
// review's M8).
func TestTheProcessListHoldsNoToken(t *testing.T) {
	dir := fstest.MapFS{
		"stat":    {Data: []byte("4242 (cloudflared) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 9 0 5000 1000 100 18446744073709551615")},
		"status":  {Data: []byte("Name:\tcloudflared\nTgid:\t4242\nUid:\t0\t0\t0\t0\n")},
		"cmdline": {Data: []byte("/usr/bin/cloudflared\x00--no-autoupdate\x00tunnel\x00run\x00--token\x00" + tokenFixture + "\x00")},
		"cgroup":  {Data: []byte("0::/system.slice/cloudflared.service\n")},
	}
	p, ok := readProcessIn(dir, 4242)
	if !ok {
		t.Fatal("not read")
	}
	if strings.Contains(p.command, tokenFixture) || strings.Contains(p.command, tokenFixture[:16]) {
		t.Fatalf("the command line holds the token: %q", p.command)
	}
	if p.command != "/usr/bin/cloudflared --no-autoupdate tunnel run --token "+Hidden || p.unit != "cloudflared.service" {
		t.Fatalf("read %q in %q", p.command, p.unit)
	}
}
