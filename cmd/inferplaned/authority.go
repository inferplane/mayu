package main

import (
	"errors"
	"strconv"

	"github.com/inferplane/inferplane/internal/adminauth"
)

func durableBudgetMode(value, policyDSN, token string) (bool, error) {
	if value == "" {
		return false, nil
	}
	on, err := strconv.ParseBool(value)
	if err != nil {
		return false, errors.New("INFERPLANED_DURABLE_BUDGETS must be true or false")
	}
	if !on {
		return false, nil
	}
	if policyDSN == "" {
		return false, errors.New("durable budgets require INFERPLANED_POLICY_DSN")
	}
	if token == "" || adminauth.IsOIDCBearerShape(token) {
		return false, errors.New("durable budgets require a non-JWT INFERPLANED_TOKEN machine credential")
	}
	return true, nil
}
