package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

func shortHash(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:12]
}

var pathParamRe = regexp.MustCompile(`\{[^}]*\}`)

func CanonicalPathTemplate(path string) string {
	return pathParamRe.ReplaceAllString(path, "{}")
}

func EndpointID(method, pathTemplate string) string {
	return "ep_" + shortHash(strings.ToUpper(method)+" "+CanonicalPathTemplate(pathTemplate))
}

func ParameterID(endpointID, location, name string) string {
	return "pa_" + shortHash(endpointID+":"+location+":"+name)
}

func RequestBodyID(endpointID string) string {
	return "rb_" + shortHash(endpointID)
}

func ResponseID(endpointID, statusCode string) string {
	return "re_" + shortHash(endpointID+":"+statusCode)
}

func NamedSchemaID(name string) string {
	return "sc_" + shortHash(name)
}

func SchemaNodeID(parentID, role string) string {
	return "sn_" + shortHash(parentID+":"+role)
}

func ServerID(url string) string {
	return "sv_" + shortHash(url)
}

func AuthSchemeID(name string) string {
	return "au_" + shortHash(name)
}

func WebhookID(event string) string {
	return "wh_" + shortHash(event)
}

func ExampleID(forNodeID, mediaType string) string {
	return "ex_" + shortHash(forNodeID+":"+mediaType)
}
