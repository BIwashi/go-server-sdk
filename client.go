package devcycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/matryer/try"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"
)

var (
	jsonCheck = regexp.MustCompile("(?i:[application|text]/json)")
	xmlCheck  = regexp.MustCompile("(?i:[application|text]/xml)")
)

// DVCClient
// In most cases there should be only one, shared, DVCClient.
type DVCClient struct {
	cfg                          *HTTPConfiguration
	common                       service // Reuse a single struct instead of allocating one for each service on the heap.
	DevCycleOptions              *DVCOptions
	sdkKey                       string
	auth                         context.Context
	localBucketing               *DevCycleLocalBucketing
	configManager                *EnvironmentConfigManager
	eventQueue                   *EventQueue
	isInitialized                bool
	internalOnInitializedChannel chan bool
}

type SDKEvent struct {
	Success             bool   `json:"success"`
	Message             string `json:"message"`
	Error               error  `json:"error"`
	FirstInitialization bool   `json:"firstInitialization"`
}

type service struct {
	client *DVCClient
}

func initializeLocalBucketing(sdkKey string, options *DVCOptions) (ret *DevCycleLocalBucketing, err error) {
	cfg := NewConfiguration(options)

	options.CheckDefaults()
	ret = &DevCycleLocalBucketing{}
	err = ret.Initialize(sdkKey, options, cfg)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	return
}

func setLBClient(sdkKey string, options *DVCOptions, c *DVCClient) error {
	localBucketing, err := initializeLocalBucketing(sdkKey, options)

	if err != nil {
		c.isInitialized = true
		if options.OnInitializedChannel != nil {
			go func() {
				options.OnInitializedChannel <- true
			}()

		}
		c.internalOnInitializedChannel <- true

		return err
	}

	c.localBucketing = localBucketing
	c.configManager = c.localBucketing.configManager
	c.eventQueue = c.localBucketing.eventQueue
	c.isInitialized = true
	if options.OnInitializedChannel != nil {
		go func() {
			options.OnInitializedChannel <- true
		}()
	}
	c.internalOnInitializedChannel <- true

	return err
}

// NewDVCClient creates a new API client.
// optionally pass a custom http.Client to allow for advanced features such as caching.
func NewDVCClient(sdkKey string, options *DVCOptions) (*DVCClient, error) {
	if sdkKey == "" {
		return nil, fmt.Errorf("Missing sdk key! Call NewDVCClient with a valid sdk key.")
	}
	if !sdkKeyIsValid(sdkKey) {
		return nil, fmt.Errorf("Invalid sdk key. Call NewDVCClient with a valid sdk key.")
	}
	cfg := NewConfiguration(options)

	options.CheckDefaults()

	c := &DVCClient{sdkKey: sdkKey}
	c.cfg = cfg
	c.common.client = c
	c.DevCycleOptions = options

	if !c.DevCycleOptions.EnableCloudBucketing {
		c.internalOnInitializedChannel = make(chan bool, 1)
		if c.DevCycleOptions.OnInitializedChannel != nil {
			go func() {
				err := setLBClient(sdkKey, options, c)
				if err != nil {
					log.Println(err.Error())
				}
			}()
		} else {
			err := setLBClient(sdkKey, options, c)
			return c, err
		}
	}
	return c, nil
}

func (c *DVCClient) generateBucketedConfig(user DVCUser) (config BucketedUserConfig, err error) {
	userJSON, err := json.Marshal(user)
	if err != nil {
		return BucketedUserConfig{}, err
	}
	config, err = c.localBucketing.GenerateBucketedConfigForUser(string(userJSON))
	if err != nil {
		return BucketedUserConfig{}, err
	}
	config.user = &user
	return
}

func (c *DVCClient) queueEvent(user DVCUser, event DVCEvent) (err error) {
	err = c.eventQueue.QueueEvent(user, event)
	return
}

func (c *DVCClient) queueAggregateEvent(bucketed BucketedUserConfig, event DVCEvent) (err error) {
	err = c.eventQueue.QueueAggregateEvent(bucketed, event)
	return
}

/*
DVCClientService Get all features by key for user data
  - @param body

@return map[string]Feature
*/
func (c *DVCClient) AllFeatures(user DVCUser) (map[string]Feature, error) {
	if !c.DevCycleOptions.EnableCloudBucketing {
		if c.hasConfig() {
			user, err := c.generateBucketedConfig(user)
			return user.Features, err
		} else {
			log.Println("AllFeatures called before client initialized")
			return map[string]Feature{}, nil
		}

	}

	populatedUser := user.getPopulatedUser()

	var (
		httpMethod          = strings.ToUpper("Post")
		postBody            interface{}
		localVarReturnValue map[string]Feature
	)

	// create path and map variables
	path := c.cfg.BasePath + "/v1/features"

	headers := make(map[string]string)
	queryParams := url.Values{}

	// body params
	postBody = &populatedUser

	r, rBody, err := c.performRequest(path, httpMethod, postBody, headers, queryParams)

	if err != nil {
		return nil, err
	}

	if r.StatusCode < 300 {
		// If we succeed, return the data, otherwise pass on to decode error.
		err = decode(&localVarReturnValue, rBody, r.Header.Get("Content-Type"))
		return localVarReturnValue, err
	}

	return nil, c.handleError(r, rBody)
}

/*
DVCClientService Get variable by key for user data
  - @param body
  - @param key Variable key

@return Variable
*/
func (c *DVCClient) Variable(userdata DVCUser, key string, defaultValue interface{}) (Variable, error) {
	if key == "" {
		return Variable{}, errors.New("invalid key provided for call to Variable")
	}

	convertedDefaultValue := convertDefaultValueType(defaultValue)
	variableType, err := variableTypeFromValue(key, convertedDefaultValue)

	if err != nil {
		return Variable{}, err
	}

	baseVar := baseVariable{Key: key, Value: convertedDefaultValue, Type_: variableType}
	variable := Variable{baseVariable: baseVar, DefaultValue: convertedDefaultValue, IsDefaulted: true}

	if !c.DevCycleOptions.EnableCloudBucketing {
		if !c.hasConfig() {
			log.Println("Variable called before client initialized, returning default value")
			return variable, nil
		}
		bucketed, err := c.generateBucketedConfig(userdata)

		sameTypeAsDefault := compareTypes(bucketed.Variables[key].Value, convertedDefaultValue)
		variableEvaluationType := ""
		if bucketed.Variables[key].Value != nil && sameTypeAsDefault {
			variable.Value = bucketed.Variables[key].Value
			variable.IsDefaulted = false
			variableEvaluationType = EventType_AggVariableEvaluated
		} else {
			if !sameTypeAsDefault && bucketed.Variables[key].Value != nil {
				log.Printf("Type mismatch for variable %s. Expected type %s, got %s",
					key,
					reflect.TypeOf(defaultValue).String(),
					reflect.TypeOf(bucketed.Variables[key].Value).String(),
				)
			}
			variableEvaluationType = EventType_AggVariableDefaulted
		}
		if !c.DevCycleOptions.DisableAutomaticEventLogging {
			err = c.queueAggregateEvent(bucketed, DVCEvent{
				Type_:  variableEvaluationType,
				Target: key,
			})
			if err != nil {
				log.Println("Error queuing aggregate event: ", err)
				err = nil
			}
		}
		return variable, err
	}

	populatedUser := userdata.getPopulatedUser()

	var (
		httpMethod          = strings.ToUpper("Post")
		postBody            interface{}
		localVarReturnValue Variable
	)

	// create path and map variables
	path := c.cfg.BasePath + "/v1/variables/{key}"
	path = strings.Replace(path, "{"+"key"+"}", fmt.Sprintf("%v", key), -1)

	headers := make(map[string]string)
	queryParams := url.Values{}

	// userdata params
	postBody = &populatedUser

	r, body, err := c.performRequest(path, httpMethod, postBody, headers, queryParams)

	if err != nil {
		return variable, err
	}

	if r.StatusCode < 300 {
		// If we succeed, return the data, otherwise pass on to decode error.
		err = decode(&localVarReturnValue, body, r.Header.Get("Content-Type"))
		if err == nil && localVarReturnValue.Value != nil {
			if compareTypes(localVarReturnValue.Value, convertedDefaultValue) {
				variable.Value = localVarReturnValue.Value
				variable.IsDefaulted = false
			} else {
				log.Printf("Type mismatch for variable %s. Expected type %s, got %s",
					key,
					reflect.TypeOf(defaultValue).String(),
					reflect.TypeOf(localVarReturnValue.Value).String(),
				)
			}

			return variable, err
		}
	}

	var v ErrorResponse
	err = decode(&v, body, r.Header.Get("Content-Type"))
	if err != nil {
		log.Println(err.Error())
		return variable, nil
	}
	log.Println(v.Message)
	return variable, nil
}

func (c *DVCClient) AllVariables(user DVCUser) (map[string]ReadOnlyVariable, error) {
	var (
		httpMethod          = strings.ToUpper("Post")
		postBody            interface{}
		localVarReturnValue map[string]ReadOnlyVariable
	)
	if !c.DevCycleOptions.EnableCloudBucketing {
		if c.hasConfig() {
			user, err := c.generateBucketedConfig(user)
			if err != nil {
				return localVarReturnValue, err
			}
			return user.Variables, err
		} else {
			log.Println("AllFeatures called before client initialized")
			return map[string]ReadOnlyVariable{}, nil
		}
	}

	populatedUser := user.getPopulatedUser()

	// create path and map variables
	path := c.cfg.BasePath + "/v1/variables"

	headers := make(map[string]string)
	queryParams := url.Values{}

	// body params
	postBody = &populatedUser

	r, rBody, err := c.performRequest(path, httpMethod, postBody, headers, queryParams)
	if err != nil {
		return localVarReturnValue, err
	}

	if r.StatusCode < 300 {
		// If we succeed, return the data, otherwise pass on to decode error.
		err = decode(&localVarReturnValue, rBody, r.Header.Get("Content-Type"))
		return localVarReturnValue, err
	}

	return nil, c.handleError(r, rBody)
}

/*
DVCClientService Post events to DevCycle for user
  - @param ctx context.Context - for authentication, logging, cancellation, deadlines, tracing, etc. Passed from http.Request or context.Background().
  - @param body

@return InlineResponse201
*/

func (c *DVCClient) Track(user DVCUser, event DVCEvent) (bool, error) {
	if c.DevCycleOptions.DisableCustomEventLogging {
		return true, nil
	}
	if event.Type_ == "" {
		return false, errors.New("event type is required")
	}

	if !c.DevCycleOptions.EnableCloudBucketing {
		if c.isInitialized {
			err := c.eventQueue.QueueEvent(user, event)
			return err == nil, err
		} else {
			log.Println("Track called before client initialized")
			return true, nil
		}
	}
	var (
		httpMethod = strings.ToUpper("Post")
		postBody   interface{}
	)

	populatedUser := user.getPopulatedUser()

	events := []DVCEvent{event}
	body := UserDataAndEventsBody{User: &populatedUser, Events: events}
	// create path and map variables
	path := c.cfg.BasePath + "/v1/track"

	headers := make(map[string]string)
	queryParams := url.Values{}

	// body params
	postBody = &body

	r, rBody, err := c.performRequest(path, httpMethod, postBody, headers, queryParams)
	if err != nil {
		return false, err
	}

	if r.StatusCode < 300 {
		// If we succeed, return the data, otherwise pass on to decode error.
		err = decode(nil, rBody, r.Header.Get("Content-Type"))
		if err == nil {
			return false, err
		} else {
			return true, nil
		}
	}

	return false, c.handleError(r, rBody)
}

func (c *DVCClient) FlushEvents() error {

	if c.DevCycleOptions.EnableCloudBucketing || !c.isInitialized {
		return nil
	}

	if c.DevCycleOptions.DisableCustomEventLogging && c.DevCycleOptions.DisableAutomaticEventLogging {
		return nil
	}

	err := c.eventQueue.FlushEvents()
	return err
}

/*
Close the client and flush any pending events. Stop any ongoing tickers
*/
func (c *DVCClient) Close() (err error) {
	if c.DevCycleOptions.EnableCloudBucketing {
		return
	}

	if !c.isInitialized {
		log.Println("Awaiting client initialization before closing")
		<-c.internalOnInitializedChannel
	}

	if c.eventQueue != nil {
		err = c.eventQueue.Close()
	}

	if c.configManager != nil {
		c.configManager.Close()
	}

	return err
}

func (c *DVCClient) hasConfig() bool {
	if c.configManager == nil {
		return false
	}

	return c.configManager.hasConfig
}

func (c *DVCClient) performRequest(
	path string, method string,
	postBody interface{},
	headerParams map[string]string,
	queryParams url.Values,
) (response *http.Response, body []byte, err error) {
	headerParams["Content-Type"] = "application/json"
	headerParams["Accept"] = "application/json"
	headerParams["Authorization"] = c.sdkKey

	var httpResponse *http.Response
	var responseBody []byte

	// This retrying lib works by retrying as long as the bool is true and err is not nil
	// the attempt param is auto-incremented
	err = try.Do(func(attempt int) (bool, error) {
		var err error
		r, err := c.prepareRequest(
			path,
			method,
			postBody,
			headerParams,
			queryParams,
		)

		// Don't retry if theres an error preparing the request
		if err != nil {
			return false, err
		}

		httpResponse, err = c.callAPI(r)
		if httpResponse == nil && err == nil {
			err = errors.New("Nil httpResponse")
		}
		if err != nil {
			time.Sleep(time.Duration(exponentialBackoff(attempt)) * time.Millisecond) // wait with exponential backoff
			return attempt <= 5, err
		}
		responseBody, err = ioutil.ReadAll(httpResponse.Body)
		httpResponse.Body.Close()

		if err == nil && httpResponse.StatusCode >= 500 && attempt <= 5 {
			err = errors.New("5xx error on request")
		}

		if err != nil {
			time.Sleep(time.Duration(exponentialBackoff(attempt)) * time.Millisecond) // wait with exponential backoff
		}

		return attempt <= 5, err // try 5 times
	})

	if err != nil {
		return nil, nil, err
	}
	return httpResponse, responseBody, err

}

func (c *DVCClient) handleError(r *http.Response, body []byte) (err error) {
	newErr := GenericError{
		body:  body,
		error: r.Status,
	}

	var v ErrorResponse
	if len(body) > 0 {
		err = decode(&v, body, r.Header.Get("Content-Type"))
		if err != nil {
			newErr.error = err.Error()
			return newErr
		}
	}
	newErr.model = v

	if r.StatusCode >= 500 {
		log.Println("Request error: ", newErr)
		return nil
	}
	return newErr
}

func compareTypes(value1 interface{}, value2 interface{}) bool {
	return reflect.TypeOf(value1) == reflect.TypeOf(value2)
}

func convertDefaultValueType(value interface{}) interface{} {
	switch value.(type) {
	case int:
		return float64(value.(int))
	case int8:
		return float64(value.(int8))
	case int16:
		return float64(value.(int16))
	case int32:
		return float64(value.(int32))
	case int64:
		return float64(value.(int64))
	case uint:
		return float64(value.(uint))
	case uint8:
		return float64(value.(uint8))
	case uint16:
		return float64(value.(uint16))
	case uint32:
		return float64(value.(uint32))
	case uint64:
		return float64(value.(uint64))
	case float32:
		return float64(value.(float32))
	default:
		return value
	}
}

func variableTypeFromValue(key string, value interface{}) (varType string, err error) {
	switch value.(type) {
	case float64:
		return "Number", nil
	case string:
		return "String", nil
	case bool:
		return "Boolean", nil
	case map[string]any:
		return "JSON", nil
	}

	return "", fmt.Errorf("the default value for variable %s is not of type Boolean, Number, String, or JSON", key)
}

// callAPI do the request.
func (c *DVCClient) callAPI(request *http.Request) (*http.Response, error) {
	return c.cfg.HTTPClient.Do(request)
}

func exponentialBackoff(attempt int) float64 {
	delay := math.Pow(2, float64(attempt)) * 100
	randomSum := delay * 0.2 * rand.Float64()
	return (delay + randomSum)
}

// Change base path to allow switching to mocks
func (c *DVCClient) ChangeBasePath(path string) {
	c.cfg.BasePath = path
}

func (c *DVCClient) SetOptions(dvcOptions DVCOptions) {
	c.DevCycleOptions = &dvcOptions
}

// prepareRequest build the request
func (c *DVCClient) prepareRequest(
	path string,
	method string,
	postBody interface{},
	headerParams map[string]string,
	queryParams url.Values,
) (localVarRequest *http.Request, err error) {

	var body *bytes.Buffer

	// Detect postBody type and post.
	if postBody != nil {
		contentType := headerParams["Content-Type"]
		if contentType == "" {
			contentType = detectContentType(postBody)
			headerParams["Content-Type"] = contentType
		}

		body, err = setBody(postBody, contentType)
		if err != nil {
			return nil, err
		}
	}

	// Setup path and query parameters
	url, err := url.Parse(path)
	if err != nil {
		return nil, err
	}

	// Adding Query Param
	query := url.Query()
	for k, v := range queryParams {
		for _, iv := range v {
			query.Add(k, iv)
		}
	}

	if c.DevCycleOptions.EnableEdgeDB {
		query.Add("enableEdgeDB", "true")
	}

	// Encode the parameters.
	url.RawQuery = query.Encode()

	// Generate a new request
	if body != nil {
		localVarRequest, err = http.NewRequest(method, url.String(), body)
	} else {
		localVarRequest, err = http.NewRequest(method, url.String(), nil)
	}
	if err != nil {
		return nil, err
	}

	// add header parameters, if any
	if len(headerParams) > 0 {
		headers := http.Header{}
		for h, v := range headerParams {
			headers.Set(h, v)
		}
		localVarRequest.Header = headers
	}

	// Override request host, if applicable
	if c.cfg.Host != "" {
		localVarRequest.Host = c.cfg.Host
	}

	// Add the user agent to the request.
	localVarRequest.Header.Add("User-Agent", c.cfg.UserAgent)

	for header, value := range c.cfg.DefaultHeader {
		localVarRequest.Header.Add(header, value)
	}

	return localVarRequest, nil
}

func sdkKeyIsValid(key string) bool {
	return strings.HasPrefix(key, "server") || strings.HasPrefix(key, "dvc_server")
}
