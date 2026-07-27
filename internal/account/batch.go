package account

import (
	"context"
	"errors"
	"sync"
)

const maxBatchItems = 50

type BatchTokenInput struct {
	Items []TokenInput `json:"items"`
}

type BatchItemResult struct {
	Index   int      `json:"index"`
	Status  string   `json:"status"`
	Code    string   `json:"code,omitempty"`
	Account *Account `json:"account,omitempty"`
}

type BatchTokenResult struct {
	Total     int               `json:"total"`
	Succeeded int               `json:"succeeded"`
	Failed    int               `json:"failed"`
	Results   []BatchItemResult `json:"results"`
}

func (s *Service) ImportTokenBatch(ctx context.Context, input *BatchTokenInput) (BatchTokenResult, error) {
	if input == nil || len(input.Items) == 0 {
		return BatchTokenResult{}, &ServiceError{Kind: ErrorInvalid, Code: "batch_items_required"}
	}
	if len(input.Items) > maxBatchItems {
		clearBatchInput(input)
		return BatchTokenResult{}, &ServiceError{Kind: ErrorInvalid, Code: "batch_too_large"}
	}
	items := input.Items
	input.Items = nil
	defer func() {
		for index := range items {
			clearInput(&items[index])
		}
	}()
	result := BatchTokenResult{Total: len(items), Results: make([]BatchItemResult, len(items))}
	preparedItems := make([]preparedImport, len(items))
	defer func() {
		for index := range preparedItems {
			zero(preparedItems[index].plaintext)
		}
	}()
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < 3; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if batchCredentialCount(items[index]) != 1 {
					clearInput(&items[index])
					result.Results[index] = BatchItemResult{Index: index, Status: "invalid", Code: "credential_type_conflict"}
					continue
				}
				prepared, err := s.prepare(ctx, &items[index])
				if err != nil {
					result.Results[index] = batchErrorResult(index, err)
					continue
				}
				preparedItems[index] = prepared
			}
		}()
	}
	for index := range items {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	for index, prepared := range preparedItems {
		if len(prepared.plaintext) == 0 {
			continue
		}
		accountResult, err := s.importPrepared(ctx, items[index].Label, prepared)
		zero(prepared.plaintext)
		preparedItems[index].plaintext = nil
		if err != nil {
			result.Results[index] = batchErrorResult(index, err)
			continue
		}
		result.Results[index] = BatchItemResult{Index: index, Status: "success", Account: &accountResult}
	}
	for _, item := range result.Results {
		if item.Status == "success" {
			result.Succeeded++
		} else {
			result.Failed++
		}
	}
	return result, nil
}

func batchErrorResult(index int, err error) BatchItemResult {
	result := BatchItemResult{Index: index}
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		result.Status, result.Code = "failed", "internal_error"
		return result
	}
	result.Code = serviceErr.Code
	switch serviceErr.Kind {
	case ErrorDuplicate:
		result.Status = "duplicate"
	case ErrorUnavailable:
		result.Status = "upstream_unavailable"
	case ErrorInvalid:
		result.Status = "invalid"
	default:
		result.Status, result.Code = "failed", "internal_error"
	}
	return result
}

func batchCredentialCount(input TokenInput) int {
	count := 0
	if input.AccessToken != "" {
		count++
	}
	if input.RefreshToken != "" {
		count++
	}
	if input.SessionToken != "" {
		count++
	}
	return count
}

func clearBatchInput(input *BatchTokenInput) {
	for index := range input.Items {
		clearInput(&input.Items[index])
	}
	input.Items = nil
}
