package endpoint

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/streadway/amqp"
)

var errCreateQueue = errors.New("Error while creating queue")

const (
	sqsExpiresAfter = time.Second * 30
)

// SQSConn is an endpoint connection
type SQSConn struct {
	mu      sync.Mutex
	ep      Endpoint
	session *session.Session
	svc     *sqs.SQS
	channel *amqp.Channel
	ex      bool
	t       time.Time
}

func (conn *SQSConn) generateSQSURL() string {
	return "https://sqs." + conn.ep.SQS.Region + "amazonaws.com/" + conn.ep.SQS.QueueID + "/" + conn.ep.SQS.QueueName
}

// Expired returns true if the connection has expired
func (conn *SQSConn) Expired() bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !conn.ex {
		if time.Now().Sub(conn.t) > sqsExpiresAfter {
			conn.ex = true
			conn.close()
		}
	}
	return conn.ex
}

func (conn *SQSConn) close() {
	if conn.svc != nil {
		conn.svc = nil
		conn.session = nil
	}
}

// Send sends a message
func (conn *SQSConn) Send(msg string) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.ex {
		return errExpired
	}
	conn.t = time.Now()

	if conn.svc == nil && conn.session == nil {
		credPath := conn.ep.SQS.CredPath
		credProfile := conn.ep.SQS.CredProfile
		var sess *session.Session
		if credPath != "" && credProfile != "" {
			sess = session.Must(session.NewSession(&aws.Config{
				Region:      aws.String(conn.ep.SQS.Region),
				Credentials: credentials.NewSharedCredentials(credPath, credProfile),
				MaxRetries:  aws.Int(5),
			}))
		} else if credPath != "" {
			sess = session.Must(session.NewSession(&aws.Config{
				Region:      aws.String(conn.ep.SQS.Region),
				Credentials: credentials.NewSharedCredentials(credPath, "default"),
				MaxRetries:  aws.Int(5),
			}))
		} else {
			sess = session.Must(session.NewSession(&aws.Config{
				Region:     aws.String(conn.ep.SQS.Region),
				MaxRetries: aws.Int(5),
			}))
		}
		// Create a SQS service client.
		svc := sqs.New(sess)

		svc.CreateQueue(&sqs.CreateQueueInput{
			QueueName: aws.String(conn.ep.SQS.QueueName),
			Attributes: map[string]*string{
				"DelaySeconds":           aws.String("60"),
				"MessageRetentionPeriod": aws.String("86400"),
			},
		})
		conn.session = sess
		conn.svc = svc
	}

	queueURL := conn.generateSQSURL()
	// Send message
	sendParams := &sqs.SendMessageInput{
		MessageBody: aws.String(msg),
		QueueUrl:    aws.String(queueURL),
	}
	_, err := conn.svc.SendMessage(sendParams)
	if err != nil {
		fmt.Println(err)
		return err
	}

	return nil
}

func newSQSConn(ep Endpoint) *SQSConn {
	return &SQSConn{
		ep: ep,
		t:  time.Now(),
	}
}
