package telebot

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
	"strings"
)

// A WebhookTLS specifies the path to a key and a cert so the poller can open
// a TLS listener
type WebhookTLS struct {
	Key  string
	Cert string
}

// A WebhookEndpoint describes the endpoint to which telegram will send its requests.
// This must be a public URL and can be a loadbalancer or something similar. If the
// endpoint uses TLS and the certificate is self-signed you have to add the certificate
// path of this certificate so telegram will trust it. This field can be ignored if you
// have a trusted certificate (letsencrypt, ...).
type WebhookEndpoint struct {
	PublicURL string
	Cert      string
}

// A Webhook configures the poller for webhooks. It opens a port on the given
// listen address. If TLS is filled, the listener will use the key and cert to open
// a secure port. Otherwise it will use plain HTTP.
//
// If you have a loadbalancer ore other infrastructure in front of your service, you
// must fill the Endpoint structure so this poller will send this data to telegram. If
// you leave these values empty, your local address will be sent to telegram which is mostly
// not what you want (at least while developing). If you have a single instance of your
// bot you should consider to use the LongPoller instead of a WebHook.
//
// You can also leave the Listen field empty. In this case it is up to the caller to
// add the Webhook to a http-mux.
//
type Webhook struct {
	Listen         string   `json:"url"`
	MaxConnections int      `json:"max_connections"`
	AllowedUpdates []string `json:"allowed_updates"`

	// (WebhookInfo)
	HasCustomCert  bool   `json:"has_custom_certificate"`
	PendingUpdates int    `json:"pending_update_count"`
	ErrorUnixtime  int64  `json:"last_error_date"`
	ErrorMessage   string `json:"last_error_message"`

	TLS      *WebhookTLS
	Endpoint *WebhookEndpoint

	router *gin.Engine

	// NoBotRouter is a router for no bot call its start with Endpoint PublicURL + /nobot
	NoBotRouter *gin.RouterGroup
	// noBotEndpoint is the endpoint for no bot call its start with Endpoint PublicURL + /nobot
	noBotEndpoint string

	dest chan<- Update
	bot  *Bot
}

// GetNoBotEndpoint return the base path of no bot request handlers
func (h *Webhook) GetNoBotEndpoint() string {
	return h.noBotEndpoint
}

// NewWebhook offer the possibility to handle incoming bot webhook call an no bot request use this to create a Webhook
func NewWebhook(h Webhook) *Webhook {

	publicUrl := h.Endpoint.PublicURL
	if strings.HasSuffix(publicUrl, "/") {
		h.Endpoint.PublicURL = publicUrl + "bot/"
		h.noBotEndpoint = publicUrl + "nobot"
	} else {
		h.Endpoint.PublicURL = publicUrl + "/bot/"
		h.noBotEndpoint = publicUrl + "/nobot"
	}

	gin.SetMode(gin.ReleaseMode)
	h.router = gin.Default()

	// we add this handler for prevent Docker build failed when the cloud provider want to check if the container is online by calling the root
	h.router.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, h.bot.Me.Username)
	})

	// Add handler for any type of request who come trough the bot endpoint
	h.router.Group("/bot").Any("/*action", func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
		c.String(http.StatusOK, h.bot.Me.Username)
	})
	h.NoBotRouter = h.router.Group("/nobot")
	return &h
}

func (h *Webhook) getFiles() map[string]File {
	m := make(map[string]File)

	if h.TLS != nil {
		m["certificate"] = FromDisk(h.TLS.Cert)
	}
	// check if it is overwritten by an endpoint
	if h.Endpoint != nil {
		if h.Endpoint.Cert == "" {
			// this can be the case if there is a loadbalancer or reverseproxy in
			// front with a public cert. in this case we do not need to upload it
			// to telegram. we delete the certificate from the map, because someone
			// can have an internal TLS listener with a private cert
			delete(m, "certificate")
		} else {
			// someone configured a certificate
			m["certificate"] = FromDisk(h.Endpoint.Cert)
		}
	}
	return m
}

func (h *Webhook) getParams() map[string]string {
	params := make(map[string]string)

	if h.MaxConnections != 0 {
		params["max_connections"] = strconv.Itoa(h.MaxConnections)
	}
	if len(h.AllowedUpdates) > 0 {
		data, _ := json.Marshal(h.AllowedUpdates)
		params["allowed_updates"] = string(data)
	}

	if h.TLS != nil {
		params["url"] = "https://" + h.Listen
	} else {
		// this will not work with telegram, they want TLS
		// but i allow this because telegram will send an error
		// when you register this hook. in their docs they write
		// that port 80/http is allowed ...
		params["url"] = "http://" + h.Listen
	}
	if h.Endpoint != nil {
		params["url"] = h.Endpoint.PublicURL
	}
	return params
}

func (h *Webhook) Poll(b *Bot, dest chan Update, stop chan struct{}) {
	if err := b.SetWebhook(h); err != nil {
		b.debug(err)
		close(stop)
		return
	}

	// store the variables so the HTTP-handler can use 'em
	h.dest = dest
	h.bot = b

	if h.Listen == "" {
		h.waitForStop(stop)
		return
	}

	/*s := &http.Server{
		Addr:    h.Listen,
		Handler: h,
	}*/

	go func(stop chan struct{}) {
		h.waitForStop(stop)
		//s.Shutdown(context.Background())
	}(stop)

	if h.TLS != nil {
		//s.ListenAndServeTLS(h.TLS.Cert, h.TLS.Key)
		_ = h.router.RunTLS(h.Listen, h.TLS.Cert, h.TLS.Key)
	} else {
		//s.ListenAndServe()
		_ = h.router.Run(h.Listen)
	}
}

func (h *Webhook) waitForStop(stop chan struct{}) {
	<-stop
	close(stop)
}

// The handler simply reads the update from the body of the requests
// and writes them to the update channel.
func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var update Update
	err := json.NewDecoder(r.Body).Decode(&update)
	if err != nil {
		h.bot.debug(fmt.Errorf("cannot decode update: %v", err))
		return
	}
	h.dest <- update
}

// GetWebhook returns current webhook status.
func (b *Bot) GetWebhook() (*Webhook, error) {
	data, err := b.Raw("getWebhookInfo", nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Result Webhook
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp.Result, nil
}

// SetWebhook configures a bot to receive incoming
// updates via an outgoing webhook.
func (b *Bot) SetWebhook(w *Webhook) error {
	_, err := b.sendFiles("setWebhook", w.getFiles(), w.getParams())
	return err
}

// RemoveWebhook removes webhook integration.
func (b *Bot) RemoveWebhook() error {
	_, err := b.Raw("deleteWebhook", nil)
	return err
}
