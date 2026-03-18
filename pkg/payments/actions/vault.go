package actions

import "github.com/xssnick/ton-payment-network/pkg/payments"

type vaultAction interface {
	payments.Action
	isVaultAction()
}

func IsVaultAction(action payments.Action) bool {
	_, ok := action.(vaultAction)
	return ok
}
