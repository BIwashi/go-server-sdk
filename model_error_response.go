/*
 * DevCycle Bucketing API
 *
 * Documents the DevCycle Bucketing API which provides and API interface to User Bucketing and for generated SDKs.
 *
 * API version: 1.0.1
 * Generated by: Swagger Codegen (https://github.com/swagger-api/swagger-codegen.git)
 */
package devcycle

type ErrorResponse struct {
	// Error message
	Message string `json:"message"`
	// Additional error information detailing the error reasoning
	Data *interface{} `json:"data,omitempty"`
}
