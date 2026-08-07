package monitorfacade

import (
	"context"
	"errors"
	"strings"

	allocationfacade "allocation-service/monitorfacade"
	"chatgpt-monitor/internal/account"
)

type Source interface {
	ImportByToken(context.Context, *account.TokenInput) (account.Account, error)
	List(context.Context) ([]account.Account, error)
}

type Facade struct {
	source Source
}

var _ allocationfacade.Client = (*Facade)(nil)

func New(source Source) (*Facade, error) {
	if source == nil {
		return nil, errors.New("monitor facade source is required")
	}
	return &Facade{source: source}, nil
}

func (f *Facade) ImportForAllocation(ctx context.Context, request allocationfacade.ImportRequest) (allocationfacade.ImportResult, error) {
	input, ok := tokenInput(request)
	if !ok {
		return allocationfacade.ImportResult{}, allocationfacade.NewFault(allocationfacade.FaultContractChanged)
	}
	item, err := f.source.ImportByToken(ctx, &input)
	if err != nil {
		return allocationfacade.ImportResult{}, classify(err)
	}
	return importResult(item)
}

func (f *Facade) ListAccounts(ctx context.Context) ([]allocationfacade.StatusResult, error) {
	items, err := f.source.List(ctx)
	if err != nil {
		return nil, classify(err)
	}
	result := make([]allocationfacade.StatusResult, 0, len(items))
	for _, item := range items {
		converted, err := statusResult(item)
		if err != nil {
			converted = invalidStatusResult(item)
		}
		result = append(result, converted)
	}
	return result, nil
}

func invalidStatusResult(item account.Account) allocationfacade.StatusResult {
	email := ""
	if item.Email != nil {
		email = *item.Email
	}
	errorCode := "unsupported_monitor_status"
	switch {
	case item.ProviderAccountID == "":
		errorCode = "missing_monitor_account_id"
	case item.AuthExpiry.IsZero():
		errorCode = "missing_account_expiry"
	}
	return allocationfacade.StatusResult{
		MonitorAccountID: item.ProviderAccountID,
		MonitorStatus:    item.Status,
		Email:            email,
		AccountExpiry:    item.AuthExpiry,
		Plan:             item.Plan,
		SyncErrorCode:    errorCode,
	}
}

func (f *Facade) Status(ctx context.Context, id string) (allocationfacade.ImportResult, error) {
	if strings.TrimSpace(id) == "" {
		return allocationfacade.ImportResult{}, allocationfacade.NewFault(allocationfacade.FaultContractChanged)
	}
	items, err := f.ListAccounts(ctx)
	if err != nil {
		return allocationfacade.ImportResult{}, err
	}
	for _, item := range items {
		if item.MonitorAccountID == id {
			return allocationfacade.ImportResult(item), nil
		}
	}
	return allocationfacade.ImportResult{MonitorAccountID: id, MonitorStatus: "not_found"}, nil
}

func (f *Facade) BatchStatus(ctx context.Context, ids []string) (map[string]allocationfacade.StatusResult, error) {
	items, err := f.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]allocationfacade.StatusResult, len(items))
	for _, item := range items {
		byID[item.MonitorAccountID] = item
	}
	result := make(map[string]allocationfacade.StatusResult, len(ids))
	for _, id := range ids {
		if item, ok := byID[id]; ok {
			result[id] = item
		} else {
			result[id] = allocationfacade.StatusResult{MonitorAccountID: id, MonitorStatus: "not_found"}
		}
	}
	return result, nil
}

func (f *Facade) Available(ctx context.Context) bool {
	_, err := f.ListAccounts(ctx)
	return err == nil
}

func tokenInput(request allocationfacade.ImportRequest) (account.TokenInput, bool) {
	input := account.TokenInput{Label: request.Label}
	switch request.TokenType {
	case "access_token", "access":
		input.AccessToken = request.Token
	case "refresh_token", "refresh":
		input.RefreshToken = request.Token
	case "session_token", "session":
		input.SessionToken = request.Token
	default:
		return account.TokenInput{}, false
	}
	if strings.TrimSpace(request.Token) == "" {
		return account.TokenInput{}, false
	}
	return input, true
}

func importResult(item account.Account) (allocationfacade.ImportResult, error) {
	status, err := statusResult(item)
	if err != nil {
		return allocationfacade.ImportResult{}, err
	}
	return allocationfacade.ImportResult(status), nil
}

func statusResult(item account.Account) (allocationfacade.StatusResult, error) {
	if item.ProviderAccountID == "" || item.AuthExpiry.IsZero() || !validStatus(item.Status) {
		return allocationfacade.StatusResult{}, allocationfacade.NewFault(allocationfacade.FaultContractChanged)
	}
	email := ""
	if item.Email != nil {
		email = *item.Email
	}
	return allocationfacade.StatusResult{
		MonitorAccountID: item.ProviderAccountID,
		MonitorStatus:    item.Status,
		Email:            email,
		AccountExpiry:    item.AuthExpiry,
		Plan:             item.Plan,
	}, nil
}

func validStatus(status string) bool {
	switch status {
	case "alive", "unknown", "dead_normal", "dead_banned", "not_found", "unknown_monitor":
		return true
	default:
		return false
	}
}

func classify(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return allocationfacade.NewFault(allocationfacade.FaultTimeout)
	}
	return allocationfacade.NewFault(allocationfacade.FaultUnavailable)
}
