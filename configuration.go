package elasticthought

import (
	"errors"
	"fmt"

	"github.com/couchbaselabs/logg"
	"github.com/tleyden/cbfs/client"
	"github.com/tleyden/go-couch"
)

type QueueType int

const (
	Nsq QueueType = iota
	Goroutine
)

// Holds configuration values that are used throughout the application
type Configuration struct {
	DbUrl               string
	CbfsUrl             string
	NsqLookupdUrl       string
	NsqdUrl             string
	NsqdTopic           string
	WorkDirectory       string
	QueueType           QueueType
	NumCbfsClusterNodes int // needed to validate cbfs cluster health
}

func NewDefaultConfiguration() *Configuration {

	config := &Configuration{
		DbUrl:               "http://localhost:4985/elastic-thought",
		CbfsUrl:             "http://localhost:8484",
		NsqLookupdUrl:       "127.0.0.1:4161",
		NsqdUrl:             "127.0.0.1:4150",
		NsqdTopic:           "elastic-thought",
		WorkDirectory:       "/tmp/elastic-thought",
		QueueType:           Goroutine,
		NumCbfsClusterNodes: 1,
	}
	return config

}

// Connect to db based on url stored in config, or panic if not able to connect
func (c Configuration) DbConnection() couch.Database {
	db, err := couch.Connect(c.DbUrl)
	if err != nil {
		err = errors.New(fmt.Sprintf("Error %v | dbUrl: %v", err, c.DbUrl))
		logg.LogPanic("%v", err)
	}
	return db
}

// Create a new cbfs client based on url stored in config
func (c Configuration) NewCbfsClient() (*cbfsclient.Client, error) {
	return cbfsclient.New(c.CbfsUrl)
}
