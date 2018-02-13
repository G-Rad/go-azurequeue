package queue

import (
	"net/http"
	"fmt"
	"time"
	"bytes"
	"strconv"
	"strings"
	"net/url"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io/ioutil"
	"sync"
)

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

var httpClientOverride httpClient = nil

// Sets the package's http client.
func SetHttpClient(client httpClient) {
	httpClientOverride = client
}

// Queue Message.
//
// See https://docs.microsoft.com/en-us/rest/api/servicebus/message-headers-and-properties
type Message struct {
	ContentType             string
	CorrelationId           string
	SessionId               string
	DeliveryCount           int
	LockedUntilUtc          time.Time
	LockToken               string
	Id                      string
	Label                   string
	ReplyTo                 string
	EnqueuedTimeUtc         time.Time
	SequenceNumber          int64
	TimeToLive              int
	To                      string
	ScheduledEnqueueTimeUtc time.Time
	ReplyToSessionId        string
	PartitionKey            string

	Properties map[string]string

	Body []byte
}

// Thread-safe client for Azure Service Bus Queue.
type QueueClient struct {

	// Service Bus Namespace e.g. https://<yournamespace>.servicebus.windows.net
	Namespace string

	// Policy name e.g. RootManageSharedAccessKey
	KeyName string

	// Policy value.
	KeyValue string

	// Name of the queue.
	QueueName string

	// Request timeout in seconds.
	Timeout int

	mu         sync.Mutex
	httpClient httpClient
}

// This operation atomically retrieves and locks a message from a queue or subscription for processing.
// The message is guaranteed not to be delivered to other receivers (on the same queue or subscription only) during the
// lock duration specified in the queue description.
// When the lock expires, the message becomes available to other receivers.
// In order to complete processing of the message, the receiver should issue a delete command with the
// lock ID received from this operation. To abandon processing of the message and unlock it for other receivers,
// an Unlock Message command should be issued, otherwise the lock duration period can expire.

// For more information see https://docs.microsoft.com/en-us/rest/api/servicebus/peek-lock-message-non-destructive-read
func (q *QueueClient) GetMessage() (*Message, error) {

	req, err := q.createRequest("messages/head?timeout="+strconv.Itoa(q.Timeout), "POST")

	if err != nil {
		return nil, wrap(err, "Request create failed")
	}
	resp, err := q.getClient().Do(req)

	if err != nil {
		return nil, wrap(err, "Sending POST createRequest failed")
	}

	defer resp.Body.Close()

	if err := handleStatusCode(resp); err != nil {
		return nil, err
	}

	return parseMessage(resp)
}

// Sends message to a Service Bus queue.
func (q *QueueClient) SendMessage(msg *Message) error {
	req, err := q.createRequestFromMessage("messages/", "POST", msg)

	if err != nil {
		return wrap(err, "Request create failed")
	}

	resp, err := q.getClient().Do(req)

	if err != nil {
		return wrap(err, "Sending POST createRequest failed")
	}

	defer resp.Body.Close()

	return handleStatusCode(resp)
}

// Unlocks a message for processing by other receivers on a specified subscription.
// This operation deletes the lock object, causing the message to be unlocked.
// Before the operation is called, a receiver must first lock the message.
//
// For more information see https://docs.microsoft.com/en-us/rest/api/servicebus/unlock-message
func (q *QueueClient) UnlockMessage(msg *Message) error {
	req, err := q.createRequest("messages/"+msg.Id+"/"+msg.LockToken, "PUT")

	if err != nil {
		return wrap(err, "Request create failed")
	}

	resp, err := q.getClient().Do(req)

	if err != nil {
		return wrap(err, "Sending PUT createRequest failed")
	}

	defer resp.Body.Close()

	return handleStatusCode(resp)
}

// This operation completes the processing of a locked message and deletes it from the queue or subscription.
// This operation should only be called after successfully processing a previously locked message,
// in order to maintain At-Least-Once delivery assurances.
//
// For more information see https://docs.microsoft.com/en-us/rest/api/servicebus/delete-message
func (q *QueueClient) DeleteMessage(msg *Message) error {
	req, err := q.createRequest("messages/"+msg.Id+"/"+msg.LockToken, "DELETE")

	if err != nil {
		return wrap(err, "Request create failed")
	}

	resp, err := q.getClient().Do(req)

	if err != nil {
		return wrap(err, "Sending DELETE createRequest failed")
	}

	defer resp.Body.Close()

	return handleStatusCode(resp)
}

const azureQueueURL = "https://%s.servicebus.windows.net:443/%s/"

func (q *QueueClient) createRequest(path string, method string) (*http.Request, error) {
	url := fmt.Sprintf(azureQueueURL, q.Namespace, q.QueueName) + path

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", q.makeAuthHeader(url, time.Now()))
	return req, nil
}

func (q *QueueClient) createRequestFromMessage(path string, method string, msg *Message) (*http.Request, error) {
	url := fmt.Sprintf(azureQueueURL, q.Namespace, q.QueueName) + path

	req, err := http.NewRequest(method, url, bytes.NewBuffer(msg.Body))
	if err != nil {
		return nil, err
	}

	for k, v := range msg.Properties {
		req.Header.Add(k, v)
	}

	req.Header.Set("Authorization", q.makeAuthHeader(url, time.Now()))
	return req, nil
}

func (q *QueueClient) getClient() httpClient {

	if httpClientOverride != nil {
		return httpClientOverride
	}

	if q.httpClient != nil {
		return q.httpClient
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.httpClient == nil {
		q.httpClient = &http.Client{}
	}

	return q.httpClient
}

// Creates an authenticaiton header with Shared Access Signature token.
//
// For more information see: https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-sas
func (q *QueueClient) makeAuthHeader(uri string, from time.Time) string {

	const expireInSeconds = 300

	epoch := from.Add(expireInSeconds * time.Second).Round(time.Second).Unix()
	expiry := strconv.Itoa(int(epoch))

	// as per https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-sas
	encodedUri := strings.ToLower(url.QueryEscape(uri))
	sig := q.makeSignatureString(encodedUri + "\n" + expiry)
	return fmt.Sprintf("SharedAccessSignature sig=%s&se=%s&skn=%s&sr=%s", sig, expiry, q.KeyName, encodedUri)
}

// Returns SHA-256 hash of the scope of the token with a CRLF appended and an expiry time.
func (q *QueueClient) makeSignatureString(s string) string {
	// as per https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-sas
	h := hmac.New(sha256.New, []byte(q.KeyValue))
	h.Write([]byte(s))
	encodedSig := base64.StdEncoding.EncodeToString(h.Sum(nil))
	return url.QueryEscape(encodedSig)
}

func handleStatusCode(resp *http.Response) error {

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}

	body, _ := ioutil.ReadAll(resp.Body)

	switch resp.StatusCode {
	case 204:
		return NoMessagesAvailableError{204, string(body)}
	case 400:
		return BadRequestError{400, string(body)}
	case 401:
		return NotAuthorizedError{401, string(body)}
	case 404:
		return MessageDontExistError{404, string(body)}
	case 410:
		return QueueDontExistError{410, string(body)}
	case 500:
		return InternalError{500, string(body)}
	}

	return fmt.Errorf("Unknown status %v with body %v", resp.StatusCode, body)
}

func parseMessage(resp *http.Response) (*Message, error) {

	logger.Debug("Response StatusCode ", resp.StatusCode)
	logger.Debug("Response Status ", resp.Status)
	logger.Debug("Response Header ", resp.Header)
	logger.Debug("Response ContentLength ", resp.ContentLength)

	m := Message{}
	m.Properties = map[string]string{}

	for k, v := range resp.Header {
		if k != "BrokerProperties" {
			m.Properties[k] = v[0]
		}
	}

	brokerProperties := resp.Header.Get("BrokerProperties")

	if len(brokerProperties) > 0 {
		parseBrokerProperties(&m, brokerProperties)
	}

	value, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return nil, wrap(err, "Error reading message body")
	}

	m.Body = value

	return &m, nil
}

func parseBrokerProperties(m *Message, properties string) {

	logger.Debug("Response BrokerProperties ", properties)

	p := brokerProperties{}
	if err := json.Unmarshal([]byte(properties), &p); err != nil {
		logger.Error("BrokerProperties header parse failed", err)
		return
	}

	m.Id = p.MessageId
	m.SessionId = p.SessionId
	m.LockToken = p.LockToken
	m.Label = p.Label
	m.ReplyTo = p.ReplyTo
	m.To = p.To
	m.ContentType = p.ContentType
	m.CorrelationId = p.CorrelationId
	m.ReplyToSessionId = p.ReplyToSessionId
	m.PartitionKey = p.PartitionKey
	m.CorrelationId = p.CorrelationId
	m.DeliveryCount = p.DeliveryCount
	m.SequenceNumber = p.SequenceNumber
	m.TimeToLive = p.TimeToLive

	const Rfc2616Time = "Mon, 02 Jan 2006 15:04:05 MST"

	if t, err := time.Parse(Rfc2616Time, p.LockedUntilUtc); err == nil {
		m.LockedUntilUtc = t
	}

	if t, err := time.Parse(Rfc2616Time, p.EnqueuedTimeUtc); err == nil {
		m.EnqueuedTimeUtc = t
	}

	if t, err := time.Parse(Rfc2616Time, p.ScheduledEnqueueTimeUtc); err == nil {
		m.ScheduledEnqueueTimeUtc = t
	}

}

// See https://docs.microsoft.com/en-us/rest/api/servicebus/message-headers-and-properties
type brokerProperties struct {
	MessageId               string `json:"MessageId"`
	LockToken               string `json:"LockToken"`
	Label                   string `json:"Label"`
	ContentType             string `json:"ContentType"`
	CorrelationId           string `json:"CorrelationId"`
	SessionId               string `json:"SessionId"`
	DeliveryCount           int    `json:"DeliveryCount"`
	LockedUntilUtc          string `json:"LockedUntilUtc"`
	ReplyTo                 string `json:"ReplyTo"`
	EnqueuedTimeUtc         string `json:"EnqueuedTimeUtc"`
	SequenceNumber          int64  `json:"SequenceNumber"`
	TimeToLive              int    `json:"TimeToLive"`
	To                      string `json:"To"`
	ScheduledEnqueueTimeUtc string `json:"ScheduledEnqueueTimeUtc"`
	ReplyToSessionId        string `json:"ReplyToSessionId"`
	PartitionKey            string `json:"PartitionKey"`
}
