export default function handler(http) {
	site = site + "!"
	fresh = "new"
	http.vars.from_js = "site=" + site + " host=" + host + " caddy=" + (http.vars.from_caddy ?? "missing")
	http.vars.explicit = fresh
}
