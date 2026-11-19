export default function handler(http) {
	http.rewrite({ path: "/lib.js" })
}
