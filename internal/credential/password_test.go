package credential

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGeneratePassword(t *testing.T) {
	first, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("random passwords repeated")
	}
	for _, password := range []string{first, second} {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(password)
		if err != nil || len(decoded) != 32 || len(password) != 43 || strings.ContainsAny(password, "+/=") {
			t.Fatalf("generated password has invalid length or encoding: %v", err)
		}
	}
}

func TestHashUsesIndependentSaltAndVerifies(t *testing.T) {
	password := " administrator-password "
	first, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, phcPrefix) || !strings.HasPrefix(second, phcPrefix) {
		t.Fatal("password hash has an unexpected profile")
	}
	firstSalt := strings.Split(first, "$")[4]
	secondSalt := strings.Split(second, "$")[4]
	if first == second || firstSalt == secondSalt {
		t.Fatal("password hashing reused its salt")
	}
	decodedSalt, err := base64.RawStdEncoding.Strict().DecodeString(firstSalt)
	if err != nil || len(decodedSalt) != 16 {
		t.Fatalf("invalid random salt: %v", err)
	}
	valid, err := VerifyPassword(password, first)
	if err != nil || !valid {
		t.Fatalf("correct password did not verify: %v", err)
	}
	for _, wrong := range []string{"different-password", strings.TrimSpace(password)} {
		valid, err = VerifyPassword(wrong, first)
		if err != nil || valid {
			t.Fatalf("incorrect or trimmed password verified or caused a format error: %v", err)
		}
	}
}

func TestPasswordByteLengthBounds(t *testing.T) {
	for _, password := range []string{"", strings.Repeat("x", 11), strings.Repeat("x", 1025), strings.Repeat("密", 342)} {
		if _, err := HashPassword(password); err == nil {
			t.Fatal("hashed password outside the byte length limits")
		}
		if valid, err := VerifyPassword(password, "invalid"); valid || err == nil {
			t.Fatal("verified password outside the byte length limits")
		}
	}
	for _, password := range []string{strings.Repeat("x", 12), strings.Repeat("x", 1024), strings.Repeat("密", 4)} {
		encoded, err := HashPassword(password)
		if err != nil {
			t.Fatalf("valid byte length rejected: %v", err)
		}
		if valid, err := VerifyPassword(password, encoded); err != nil || !valid {
			t.Fatalf("password at valid byte length did not verify: %v", err)
		}
	}
}

func TestVerifyRejectsMalformedAndUnboundedPHC(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString(make([]byte, saltBytes))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, keyBytes))
	encoded := phcPrefix + salt + "$" + key
	cases := map[string]string{
		"empty":             "",
		"plain text":        "administrator-password",
		"too long":          strings.Repeat("x", maxEncodedBytes+1),
		"unknown algorithm": strings.Replace(encoded, "argon2id", "argon2i", 1),
		"unknown version":   strings.Replace(encoded, "v=19", "v=20", 1),
		"missing version":   strings.Replace(encoded, "v=19$", "", 1),
		"huge memory":       strings.Replace(encoded, "m=65536", "m=4294967295", 1),
		"zero memory":       strings.Replace(encoded, "m=65536", "m=0", 1),
		"lower memory":      strings.Replace(encoded, "m=65536", "m=32768", 1),
		"negative memory":   strings.Replace(encoded, "m=65536", "m=-1", 1),
		"zero iterations":   strings.Replace(encoded, "t=2", "t=0", 1),
		"higher iterations": strings.Replace(encoded, "t=2", "t=999999999", 1),
		"zero parallelism":  strings.Replace(encoded, "p=1", "p=0", 1),
		"more parallelism":  strings.Replace(encoded, "p=1", "p=255", 1),
		"unknown parameter": strings.Replace(encoded, "p=1", "p=1,x=0", 1),
		"duplicate memory":  strings.Replace(encoded, "m=65536", "m=65536,m=65536", 1),
		"reordered costs":   strings.Replace(encoded, "m=65536,t=2,p=1", "t=2,m=65536,p=1", 1),
		"extra field":       encoded + "$extra",
		"missing key":       phcPrefix + salt,
		"padded salt":       phcPrefix + salt + "==$" + key,
		"short salt":        phcPrefix + salt[:len(salt)-1] + "$" + key,
		"long salt":         phcPrefix + salt + "A$" + key,
		"invalid salt":      phcPrefix + "!" + salt[1:] + "$" + key,
		"salt unused bits":  phcPrefix + salt[:len(salt)-1] + "B$" + key,
		"salt newline":      phcPrefix + salt + "\n$" + key,
		"salt CRLF":         phcPrefix + salt[:len(salt)-2] + "\r\n$" + key,
		"padded key":        phcPrefix + salt + "$" + key + "=",
		"short key":         phcPrefix + salt + "$" + key[:len(key)-1],
		"long key":          phcPrefix + salt + "$" + key + "A",
		"invalid key":       phcPrefix + salt + "$!" + key[1:],
		"key unused bits":   phcPrefix + salt + "$" + key[:len(key)-1] + "B",
		"key newline":       encoded + "\n",
		"key CRLF":          phcPrefix + salt + "$" + key[:len(key)-2] + "\r\n",
	}
	for name, malformed := range cases {
		t.Run(name, func(t *testing.T) {
			valid, err := VerifyPassword("administrator-password", malformed)
			if err == nil || valid {
				t.Fatal("accepted an invalid or unsupported password hash")
			}
			if strings.Contains(err.Error(), malformed) && malformed != "" {
				t.Fatal("hash error echoed its input")
			}
		})
	}
}

func TestErrorsDoNotEchoPasswordOrHash(t *testing.T) {
	password := "secret12345"
	if _, err := HashPassword(password); err == nil || strings.Contains(err.Error(), password) {
		t.Fatal("password validation error exposed its input")
	}
	encoded := "malformed-secret-hash"
	if valid, err := VerifyPassword("administrator-password", encoded); valid || err == nil || strings.Contains(err.Error(), encoded) {
		t.Fatal("hash validation error exposed its input")
	}
}
