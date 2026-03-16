package azsdk

import (
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

type TokenClaims struct {
	ObjectID string
	TenantID string
	Groups   []string
	IDType   string
}

func GetTokenClaims(token string) (*TokenClaims, error) {
	p := jwt.NewParser()

	parsed, _, err := p.ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("parsing token: %w", err)
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("invalid type for claims")
	}

	var res TokenClaims

	// claims doc: https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims-reference

	if res.IDType, err = getStringClaim(claims, "idtyp"); err != nil {
		return nil, err
	}

	if res.ObjectID, err = getStringClaim(claims, "oid"); err != nil {
		return nil, err
	}

	if res.TenantID, err = getStringClaim(claims, "tid"); err != nil {
		return nil, err
	}

	if res.Groups, err = getStringSlice(claims, "groups"); err != nil {
		return nil, err
	}

	return &res, nil
}

func getStringClaim(claims jwt.MapClaims, field string) (string, error) {
	v, ok := claims[field]
	if !ok {
		return "", nil
	}

	vStr, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid type for field %q", field)
	}

	return vStr, nil
}

func getStringSlice(claims jwt.MapClaims, field string) ([]string, error) {
	v, ok := claims[field]
	if !ok {
		return nil, nil
	}

	vSlice, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid type for field %q", field)
	}

	var res []string

	for _, group := range vSlice {
		id, ok := group.(string)
		if !ok {
			return nil, fmt.Errorf("unable to cast group ID to string: %v", group)
		}

		res = append(res, id)
	}

	return res, nil
}
