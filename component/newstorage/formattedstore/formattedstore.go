/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package formattedstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hyperledger/aries-framework-go/pkg/common/log"
	"github.com/hyperledger/aries-framework-go/pkg/newstorage"
)

const (
	expressionTagNameOnlyLength     = 1
	expressionTagNameAndValueLength = 2

	failFormat                     = `failed to format %s "%s": %w`
	failFormatData                 = `failed to format data: %w`
	failDeformat                   = `failed to deformat %s "%s" returned from the underlying store: %w`
	failQueryUnderlyingStore       = "failed to query underlying store: %w"
	failGetValueUnderlyingIterator = "failed to get formatted value from the underlying iterator: %w"
)

var (
	logger = log.New("formattedstore")

	errEmptyKey                     = errors.New("key cannot be empty")
	errInvalidQueryExpressionFormat = errors.New("invalid expression format. " +
		"it must be in the following format: TagName:TagValue")
)

// Formatter represents a type that can convert data between two formats.
type Formatter interface {
	Format(key string, value []byte, tags ...newstorage.Tag) (formattedKey string, formattedValue []byte,
		formattedTags []newstorage.Tag, err error)
	Deformat(formattedKey string, formattedValue []byte, formattedTags ...newstorage.Tag) (key string, value []byte,
		tags []newstorage.Tag, err error)
}

// FormattedProvider is a newstorage.Provider that allows for data to be formatted in an underlying provider.
type FormattedProvider struct {
	provider      newstorage.Provider
	cacheProvider newstorage.Provider
	openStores    map[string]*formatStore
	formatter     Formatter
	lock          sync.RWMutex
}

// Option represents an option for a FormattedProvider.
type Option func(opts *FormattedProvider)

// WithCacheProvider option enables a cache for a FormatProvider. This cache will hold unformatted data, which can
// result in a performance improvement in cases where the formatting operation is expensive. Tags are never cached,
// so any retrieval operations that require them will always skip the cache. They cannot be cached efficiently from
// store.Get calls since getting tags would require a separate call to the main provider, and Query calls have to skip
// the cache anyway in order to make sure that no uncached data is missed.
func WithCacheProvider(cacheProvider newstorage.Provider) Option {
	return func(formattedProvider *FormattedProvider) {
		formattedProvider.cacheProvider = cacheProvider
	}
}

// NewProvider instantiates a new FormattedProvider with the given newstorage.Provider and Formatter.
// The Formatter is used to format data before being sent to the Provider for storage.
// The Formatter is also used to restore the original format of data being retrieved from Provider.
func NewProvider(underlyingProvider newstorage.Provider, formatter Formatter, options ...Option) *FormattedProvider {
	formattedProvider := &FormattedProvider{
		provider:   underlyingProvider,
		formatter:  formatter,
		openStores: make(map[string]*formatStore),
	}

	for _, option := range options {
		option(formattedProvider)
	}

	return formattedProvider
}

// OpenStore opens a store with the given name and returns a handle.
// If the store has never been opened before, then it is created.
// Store names are not case-sensitive.
func (f *FormattedProvider) OpenStore(name string) (newstorage.Store, error) {
	if name == "" {
		return nil, fmt.Errorf("store name cannot be empty")
	}

	storeName := strings.ToLower(name)

	f.lock.Lock()
	defer f.lock.Unlock()

	openStore, ok := f.openStores[storeName]
	if !ok {
		store, err := f.provider.OpenStore(storeName)
		if err != nil {
			return nil, fmt.Errorf("failed to open store in underlying provider: %w", err)
		}

		newFormatStore := formatStore{
			store:     store,
			formatter: f.formatter,
		}

		f.openStores[storeName] = &newFormatStore

		if f.cacheProvider != nil {
			cacheStore, err := f.cacheProvider.OpenStore(storeName)
			if err != nil {
				return nil, fmt.Errorf("failed to open cache store in cache provider: %w", err)
			}

			newFormatStore.cacheStore = cacheStore
		}

		return &newFormatStore, nil
	}

	return openStore, nil
}

// SetStoreConfig sets the configuration on a store.
// The store must be created prior to calling this method.
// If the store cannot be found, then an error wrapping newstorage.ErrStoreNotFound will be
// returned from the underlying provider.
func (f *FormattedProvider) SetStoreConfig(name string, config newstorage.StoreConfiguration) error {
	storeName := strings.ToLower(name)

	tags := make([]newstorage.Tag, len(config.TagNames))

	for i, tagName := range config.TagNames {
		tags[i].Name = tagName
	}

	_, _, formattedTags, err := f.formatter.Format("", nil, tags...)
	if err != nil {
		return fmt.Errorf("failed to format tag names: %w", err)
	}

	formattedTagNames := make([]string, len(config.TagNames))

	for i, formattedTag := range formattedTags {
		formattedTagNames[i] = formattedTag.Name
	}

	formattedConfig := newstorage.StoreConfiguration{TagNames: formattedTagNames}

	err = f.provider.SetStoreConfig(storeName, formattedConfig)
	if err != nil {
		return fmt.Errorf("failed to set store configuration via underlying provider: %w", err)
	}

	// The EDV Encrypted Formatter implementation cannot deformat tags directly. It needs to wrap them in
	// the store value. The code below does this, which will allow all formatters (including EDV) to function.

	store, err := f.OpenStore(storeName + "_formattedstore_storeconfig")
	if err != nil {
		return fmt.Errorf("failed to open the store config store: %w", err)
	}

	configBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config into bytes: %w", err)
	}

	err = store.Put("formattedstore_storeconfig", configBytes)
	if err != nil {
		return fmt.Errorf("failed to store config in store config store: %w", err)
	}

	if f.cacheProvider != nil {
		err = f.cacheProvider.SetStoreConfig(storeName, config)
		if err != nil {
			return fmt.Errorf("failed to set store configuration via cache provider: %w", err)
		}
	}

	return nil
}

// GetStoreConfig gets the current store configuration.
// The store must be created prior to calling this method.
// If the store cannot be found, then an error wrapping ErrStoreNotFound will be returned.
func (f *FormattedProvider) GetStoreConfig(name string) (newstorage.StoreConfiguration, error) {
	storeName := strings.ToLower(name)

	if f.cacheProvider != nil {
		config, err := f.cacheProvider.GetStoreConfig(storeName)
		if err == nil {
			return config, nil
		}

		if !errors.Is(err, newstorage.ErrStoreNotFound) {
			return newstorage.StoreConfiguration{},
				fmt.Errorf("unexpected failure while getting config from cache store: %w", err)
		}

		logger.Debugf(`Cache miss when getting store configuration for store "%s". `+
			`Will fetch data from primary provider.`, storeName)
	}

	openStore := f.openStores[storeName]
	if openStore == nil {
		return newstorage.StoreConfiguration{}, newstorage.ErrStoreNotFound
	}

	// In order to support the more restrictive EDV formatter, we bypass the usual GetStoreConfig method and instead
	// get the unformatted tags by fetching them from the "store config" formatted store created earlier.

	store, err := f.OpenStore(storeName + "_formattedstore_storeconfig")
	if err != nil {
		return newstorage.StoreConfiguration{}, fmt.Errorf("failed to open the store config store: %w", err)
	}

	configBytes, err := store.Get("formattedstore_storeconfig")
	if err != nil {
		return newstorage.StoreConfiguration{},
			fmt.Errorf("failed to get store config from the store config store: %w", err)
	}

	var config newstorage.StoreConfiguration

	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		return newstorage.StoreConfiguration{},
			fmt.Errorf("failed to unmarshal tags bytes into a tag slice: %w", err)
	}

	return config, nil
}

// GetOpenStores returns all currently open stores.
func (f *FormattedProvider) GetOpenStores() []newstorage.Store {
	f.lock.RLock()
	defer f.lock.RUnlock()

	openStores := make([]newstorage.Store, len(f.openStores))

	var counter int

	for _, openStore := range f.openStores {
		openStores[counter] = openStore
		counter++
	}

	return openStores
}

// Close closes all stores created under this store provider.
// For persistent store implementations, this does not delete any data in the underlying stores.
func (f *FormattedProvider) Close() error {
	err := f.provider.Close()
	if err != nil {
		return fmt.Errorf("failed to close underlying provider: %w", err)
	}

	if f.cacheProvider != nil {
		err := f.cacheProvider.Close()
		if err != nil {
			return fmt.Errorf("failed to close cache provider: %w", err)
		}
	}

	return nil
}

type formatStore struct {
	store      newstorage.Store
	cacheStore newstorage.Store
	formatter  Formatter
}

func (f *formatStore) Put(key string, value []byte, tags ...newstorage.Tag) error {
	if key == "" {
		return errEmptyKey
	}

	if value == nil {
		return errors.New("value cannot be nil")
	}

	formattedKey, formattedValue, formattedTags, err := f.formatter.Format(key, value, tags...)
	if err != nil {
		return fmt.Errorf(failFormatData, err)
	}

	err = f.store.Put(formattedKey, formattedValue, formattedTags...)
	if err != nil {
		return fmt.Errorf("failed to put formatted data in underlying store: %w", err)
	}

	if f.cacheStore != nil {
		err := f.cacheStore.Put(key, value)
		if err != nil {
			return fmt.Errorf("failed to put data in cache store: %w", err)
		}
	}

	return nil
}

func (f *formatStore) Get(key string) ([]byte, error) {
	if key == "" {
		return nil, errEmptyKey
	}

	if f.cacheStore != nil {
		value, err := f.cacheStore.Get(key)
		if err == nil {
			return value, nil
		}

		if !errors.Is(err, newstorage.ErrDataNotFound) {
			return nil,
				fmt.Errorf("unexpected failure while getting data from cache store: %w", err)
		}

		logger.Debugf(`cache miss when getting value for key "%s" from cache storage provider. `+
			`Will fetch data from primary provider.`, key)
	}

	formattedKey, _, _, err := f.formatter.Format(key, nil, nil...)
	if err != nil {
		return nil, fmt.Errorf(failFormat, "key", key, err)
	}

	formattedValue, err := f.store.Get(formattedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get formatted value from underlying store: %w", err)
	}

	_, value, _, err := f.formatter.Deformat("", formattedValue)
	if err != nil {
		return nil, fmt.Errorf(failDeformat, "value", string(formattedValue), err)
	}

	if f.cacheStore != nil {
		err := f.cacheStore.Put(key, value)
		if err != nil {
			return nil, fmt.Errorf("failed to put the newly retrieved value into the cache store: %w", err)
		}
	}

	return value, nil
}

func (f *formatStore) GetTags(key string) ([]newstorage.Tag, error) {
	formattedKey, _, _, err := f.formatter.Format(key, nil)
	if err != nil {
		return nil, fmt.Errorf(failFormat, "key", key, err)
	}

	formattedTags, err := f.store.GetTags(formattedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get formatted tags from underlying store: %w", err)
	}

	// FormatProvider must support EDV formatting, and EDV tags are not reversible since they are hashed.
	// Retrieving EDV tags requires embedding them in the stored value itself.
	// In order to support this use case, formatStore also calls the underlying store's Get method.
	formattedValue, err := f.store.Get(formattedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get formatted tags from underlying store: %w", err)
	}

	_, _, tags, err := f.formatter.Deformat("", formattedValue, formattedTags...)
	if err != nil {
		return nil, fmt.Errorf("failed to deformat tags: %w", err)
	}

	return tags, nil
}

// TODO (#2476): Add caching support to GetBulk method.
func (f *formatStore) GetBulk(keys ...string) ([][]byte, error) {
	formattedKeys := make([]string, len(keys))

	for i, key := range keys {
		var err error

		formattedKeys[i], _, _, err = f.formatter.Format(key, nil)
		if err != nil {
			return nil, fmt.Errorf(failFormat, "key", key, err)
		}
	}

	formattedValues, err := f.store.GetBulk(formattedKeys...)
	if err != nil {
		return nil, fmt.Errorf("failed to get formatted values from underlying store: %w", err)
	}

	deformattedValues := make([][]byte, len(formattedValues))

	for i, formattedValue := range formattedValues {
		if formattedValue != nil {
			_, deformattedValue, _, err := f.formatter.Deformat("", formattedValue, nil...)
			if err != nil {
				return nil, fmt.Errorf(failDeformat, "value", formattedValue, err)
			}

			deformattedValues[i] = deformattedValue
		}
	}

	return deformattedValues, nil
}

func (f *formatStore) Query(expression string, options ...newstorage.QueryOption) (newstorage.Iterator, error) {
	if expression == "" {
		return &formattedIterator{}, errInvalidQueryExpressionFormat
	}

	expressionSplit := strings.Split(expression, ":")
	switch len(expressionSplit) {
	case expressionTagNameOnlyLength:
		_, _, formattedTags, err := f.formatter.Format("", nil, newstorage.Tag{Name: expressionSplit[0]})
		if err != nil {
			return &formattedIterator{}, fmt.Errorf(failFormat, "tag name", expressionSplit[0], err)
		}

		underlyingIterator, err := f.store.Query(formattedTags[0].Name, options...)
		if err != nil {
			return &formattedIterator{}, fmt.Errorf(failQueryUnderlyingStore, err)
		}

		return &formattedIterator{underlyingIterator: underlyingIterator, formatter: f.formatter}, nil
	case expressionTagNameAndValueLength:
		_, _, formattedTags, err := f.formatter.Format("", nil,
			newstorage.Tag{Name: expressionSplit[0], Value: expressionSplit[1]})
		if err != nil {
			return &formattedIterator{}, fmt.Errorf("failed to format tag: %w", err)
		}

		underlyingIterator, err := f.store.Query(
			fmt.Sprintf("%s:%s", formattedTags[0].Name, formattedTags[0].Value), options...)
		if err != nil {
			return &formattedIterator{}, fmt.Errorf(failQueryUnderlyingStore, err)
		}

		return &formattedIterator{underlyingIterator: underlyingIterator, formatter: f.formatter}, nil
	default:
		return &formattedIterator{}, errInvalidQueryExpressionFormat
	}
}

func (f *formatStore) Delete(key string) error {
	formattedKey, _, _, err := f.formatter.Format(key, nil, nil...)
	if err != nil {
		return fmt.Errorf(failFormat, "key", key, err)
	}

	err = f.store.Delete(formattedKey)
	if err != nil {
		return fmt.Errorf("failed to delete data in underlying store: %w", err)
	}

	if f.cacheStore != nil {
		err := f.cacheStore.Delete(key)
		if err != nil {
			return fmt.Errorf("failed to delete data in cache store: %w", err)
		}
	}

	return nil
}

// TODO (#2476): Add caching support to Batch method.
func (f *formatStore) Batch(operations []newstorage.Operation) error {
	formattedOperations := make([]newstorage.Operation, len(operations))

	for i, operation := range operations {
		formattedKey, formattedValue, formattedTags, err :=
			f.formatter.Format(operation.Key, operation.Value, operation.Tags...)
		if err != nil {
			return fmt.Errorf(failFormatData, err)
		}

		if operation.Value == nil {
			// Ensure that, even if the formatter output a non-nil value,
			// the "nil value = delete" semantics defined in newstorage.Store are followed.
			formattedOperations[i] = newstorage.Operation{
				Key:  formattedKey,
				Tags: formattedTags,
			}

			continue
		}

		formattedOperations[i] = newstorage.Operation{
			Key:   formattedKey,
			Value: formattedValue,
			Tags:  formattedTags,
		}
	}

	err := f.store.Batch(formattedOperations)
	if err != nil {
		return fmt.Errorf("failed to perform formatted operations in underlying store: %w", err)
	}

	if f.cacheStore != nil {
		err := f.cacheStore.Batch(operations)
		if err != nil {
			return fmt.Errorf("failed to perform operations in cache store: %w", err)
		}
	}

	return nil
}

func (f *formatStore) Flush() error {
	err := f.store.Flush()
	if err != nil {
		return fmt.Errorf("failed to flush underlying store: %w", err)
	}

	return nil
}

func (f *formatStore) Close() error {
	err := f.store.Close()
	if err != nil {
		return fmt.Errorf("failed to close underlying store: %w", err)
	}

	if f.cacheStore != nil {
		err := f.cacheStore.Close()
		if err != nil {
			return fmt.Errorf("failed to close cache store: %w", err)
		}
	}

	return nil
}

type formattedIterator struct {
	underlyingIterator newstorage.Iterator
	formatter          Formatter
}

func (f *formattedIterator) Next() (bool, error) {
	nextOK, err := f.underlyingIterator.Next()
	if err != nil {
		return false, fmt.Errorf("failed to move the entry pointer in the underlying iterator: %w", err)
	}

	return nextOK, nil
}

func (f *formattedIterator) Key() (string, error) {
	formattedKey, err := f.underlyingIterator.Key()
	if err != nil {
		return "", fmt.Errorf("failed to get formatted key from the underlying iterator: %w", err)
	}

	// Some Formatter implementations (like EDV Encrypted Formatter) require the value to determine the deformatted key.
	formattedValue, err := f.underlyingIterator.Value()
	if err != nil {
		return "", fmt.Errorf(failGetValueUnderlyingIterator, err)
	}

	key, _, _, err := f.formatter.Deformat(formattedKey, formattedValue, nil...)
	if err != nil {
		return "", fmt.Errorf("failed to deformat formatted key from the underlying iterator: %w", err)
	}

	return key, nil
}

func (f *formattedIterator) Value() ([]byte, error) {
	formattedValue, err := f.underlyingIterator.Value()
	if err != nil {
		return nil, fmt.Errorf(failGetValueUnderlyingIterator, err)
	}

	_, value, _, err := f.formatter.Deformat("", formattedValue)
	if err != nil {
		return nil, fmt.Errorf(failDeformat, "value", string(formattedValue), err)
	}

	return value, nil
}

func (f *formattedIterator) Tags() ([]newstorage.Tag, error) {
	formattedTags, err := f.underlyingIterator.Tags()
	if err != nil {
		return nil, fmt.Errorf("failed to get formatted tags from the underlying iterator: %w", err)
	}

	// FormatProvider must support EDV formatting, and EDV tags are not reversible since they are hashed.
	// Retrieving EDV tags requires embedding them in the stored value itself.
	// In order to support this use case, formatStore calls the underlying iterator's Value method as well.
	formattedValue, err := f.underlyingIterator.Value()
	if err != nil {
		return nil, fmt.Errorf(failGetValueUnderlyingIterator, err)
	}

	_, _, tags, err := f.formatter.Deformat("", formattedValue, formattedTags...)
	if err != nil {
		return nil, fmt.Errorf(failDeformat, "value", string(formattedValue), err)
	}

	return tags, nil
}

func (f *formattedIterator) Close() error {
	err := f.underlyingIterator.Close()
	if err != nil {
		return fmt.Errorf("failed to close underlying iterator: %w", err)
	}

	return nil
}
