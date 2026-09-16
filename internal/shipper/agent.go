package shipper

// How the agent names itself in the requests it makes (ADR 0083). A version
// describes the sender rather than the cluster, so it belongs to the request
// and not to a payload — and a reader of the request needs no payload open to
// know which build is talking.

// product is what the agent calls itself, after its repository and its chart.
const product = "runtime-agent"

// maxVersion bounds the version stated. The build stamps it from a tag, which
// is a string this repository does not control, so the length it may reach is
// decided here rather than by whatever tagged the commit.
const maxVersion = 64

// userAgent is what this build states about itself: the product and its
// version, or the product alone when the version cannot be stated.
//
// Naming ourselves without a version is the honest half-answer, and it beats
// both alternatives: an unstated version reads as one that was never sent,
// which is a case any reader already handles, while a version carrying
// whatever a tag happened to hold would be a claim made in a shape nothing
// agreed to.
func userAgent(version string) string {
	if version == "" || len(version) > maxVersion || !plainVersion(version) {
		return product
	}
	return product + "/" + version
}

// plainVersion reports whether a version can be stated as it is: what a
// semantic version is made of, plus the letters that spell the `dev` an
// unstamped build carries.
func plainVersion(version string) bool {
	for _, r := range version {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == '.', r == '-', r == '+', r == '_':
		default:
			return false
		}
	}
	return true
}
