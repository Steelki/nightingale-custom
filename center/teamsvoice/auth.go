package teamsvoice

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"sync"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/golang-jwt/jwt/v5"
)

const tenantClaim = "http://schemas.microsoft.com/identity/claims/tenantid"

// 固定微软可信元数据入口；不能接受JWT中提供的jku/x5u作为密钥来源。
const metadataURL = "https://api.aps.skype.com/v1/.well-known/OpenIdConfiguration"

type Verifier struct {
	Client  Client
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

var DefaultVerifier = &Verifier{}

func (v *Verifier) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Since(v.fetched) < time.Hour {
		if k := v.keys[kid]; k != nil {
			return k, nil
		}
		if time.Since(v.fetched) < time.Minute {
			return nil, errors.New("unknown signing key")
		}
	}
	raw, _, e := v.Client.request(ctx, "GET", metadataURL, nil, nil)
	if e != nil {
		return nil, e
	}
	var meta struct {
		JWKS string `json:"jwks_uri"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return nil, errors.New("invalid OIDC metadata")
	}
	u, e := url.Parse(meta.JWKS)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("invalid JWKS URL")
	}
	raw, _, e = v.Client.request(ctx, "GET", meta.JWKS, nil, nil)
	if e != nil {
		return nil, e
	}
	var doc struct {
		Keys []struct{ Kty, Kid, N, E, Use, Alg string }
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, errors.New("invalid JWKS")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, e1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, e2 := base64.RawURLEncoding.DecodeString(k.E)
		if e1 != nil || e2 != nil || len(n) < 256 || len(eb) > 4 {
			continue
		}
		exp := 0
		for _, b := range eb {
			exp = exp*256 + int(b)
		}
		if exp < 3 || exp%2 == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	v.keys = keys
	v.fetched = time.Now()
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}
func (v *Verifier) Verify(ctx context.Context, raw string, cfg models.TeamsVoiceConfig) error {
	claims := jwt.MapClaims{}
	token, e := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		return v.signingKey(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithAudience(cfg.AppID), jwt.WithExpirationRequired(), jwt.WithLeeway(30*time.Second))
	if e != nil || !token.Valid {
		return errors.New("invalid callback token")
	}
	issuer, _ := claims.GetIssuer()
	if issuer != "https://graph.microsoft.com" && issuer != "https://api.botframework.com" {
		return errors.New("invalid callback issuer")
	}
	// .NET样例会把JWT的tid映射为长URI声明名；Go读取原始tid。
	tenant, ok := claims["tid"]
	if !ok {
		tenant = claims[tenantClaim]
	}
	if tenant != cfg.TenantID || (claims[tenantClaim] != nil && claims[tenantClaim] != cfg.TenantID) {
		return errors.New("invalid callback tenant")
	}
	return nil
}
