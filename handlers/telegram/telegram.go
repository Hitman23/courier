package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/buger/jsonparser"
	"github.com/go-errors/errors"
	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/nyaruka/gocommon/urns"
)

func init() {
	courier.RegisterHandler(NewHandler())
}

type handler struct {
	handlers.BaseHandler
}

// NewHandler returns a new TelegramHandler ready to be registered
func NewHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("TG"), "Telegram")}
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	return s.AddHandlerRoute(h, http.MethodPost, "receive", h.ReceiveMessage)
}

// ReceiveMessage is our HTTP handler function for incoming messages
func (h *handler) ReceiveMessage(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	te := &telegramEnvelope{}
	err := handlers.DecodeAndValidateJSON(te, r)
	if err != nil {
		return nil, courier.WriteError(ctx, w, r, err)
	}

	// no message? ignore this
	if te.Message.MessageID == 0 {
		return nil, courier.WriteIgnored(ctx, w, r, "Ignoring request, no message")
	}

	// create our date from the timestamp
	date := time.Unix(te.Message.Date, 0).UTC()

	// create our URN
	urn := urns.NewTelegramURN(te.Message.From.ContactID, te.Message.From.Username)

	// build our name from first and last
	name := handlers.NameFromFirstLastUsername(te.Message.From.FirstName, te.Message.From.LastName, te.Message.From.Username)

	// our text is either "text" or "caption" (or empty)
	text := te.Message.Text

	// this is a start command, trigger a new conversation
	if text == "/start" {
		event := h.Backend().NewChannelEvent(channel, courier.NewConversation, urn).WithContactName(name).WithOccurredOn(date)
		err = h.Backend().WriteChannelEvent(ctx, event)
		if err != nil {
			return nil, err
		}
		return []courier.Event{event}, courier.WriteChannelEventSuccess(ctx, w, r, event)
	}

	// normal message of some kind
	if text == "" && te.Message.Caption != "" {
		text = te.Message.Caption
	}

	// deal with attachments
	mediaURL := ""
	if len(te.Message.Photo) > 0 {
		// grab the largest photo less than 100k
		photo := te.Message.Photo[0]
		for i := 1; i < len(te.Message.Photo); i++ {
			if te.Message.Photo[i].FileSize > 100000 {
				break
			}
			photo = te.Message.Photo[i]
		}
		mediaURL, err = resolveFileID(channel, photo.FileID)
	} else if te.Message.Video != nil {
		mediaURL, err = resolveFileID(channel, te.Message.Video.FileID)
	} else if te.Message.Voice != nil {
		mediaURL, err = resolveFileID(channel, te.Message.Voice.FileID)
	} else if te.Message.Sticker != nil {
		mediaURL, err = resolveFileID(channel, te.Message.Sticker.Thumb.FileID)
	} else if te.Message.Document != nil {
		mediaURL, err = resolveFileID(channel, te.Message.Document.FileID)
	} else if te.Message.Venue != nil {
		text = utils.JoinNonEmpty(", ", te.Message.Venue.Title, te.Message.Venue.Address)
		mediaURL = fmt.Sprintf("geo:%f,%f", te.Message.Location.Latitude, te.Message.Location.Longitude)
	} else if te.Message.Location != nil {
		text = fmt.Sprintf("%f,%f", te.Message.Location.Latitude, te.Message.Location.Longitude)
		mediaURL = fmt.Sprintf("geo:%f,%f", te.Message.Location.Latitude, te.Message.Location.Longitude)
	} else if te.Message.Contact != nil {
		phone := ""
		if te.Message.Contact.PhoneNumber != "" {
			phone = fmt.Sprintf("(%s)", te.Message.Contact.PhoneNumber)
		}
		text = utils.JoinNonEmpty(" ", te.Message.Contact.FirstName, te.Message.Contact.LastName, phone)
	}

	// we had an error downloading media
	if err != nil {
		return nil, courier.WriteError(ctx, w, r, errors.WrapPrefix(err, "error retrieving media", 0))
	}

	// build our msg
	msg := h.Backend().NewIncomingMsg(channel, urn, text).WithReceivedOn(date).WithExternalID(fmt.Sprintf("%d", te.Message.MessageID)).WithContactName(name)

	if mediaURL != "" {
		msg.WithAttachment(mediaURL)
	}

	// queue our message
	err = h.Backend().WriteMsg(ctx, msg)
	if err != nil {
		return nil, err
	}

	return []courier.Event{msg}, courier.WriteMsgSuccess(ctx, w, r, []courier.Msg{msg})
}

func (h *handler) sendMsgPart(msg courier.Msg, token string, path string, form url.Values, replies string) (string, *courier.ChannelLog, error) {
	// either include or remove our keyboard depending on whether we have quick replies
	if replies == "" {
		form.Add("reply_markup", `{"remove_keyboard":true}`)
	} else {
		form.Add("reply_markup", replies)
	}

	sendURL := fmt.Sprintf("%s/bot%s/%s", telegramAPIURL, token, path)
	req, err := http.NewRequest(http.MethodPost, sendURL, strings.NewReader(form.Encode()))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	rr, err := utils.MakeHTTPRequest(req)

	// build our channel log
	log := courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), rr).WithError("Message Send Error", err)

	// was this request successful?
	ok, err := jsonparser.GetBoolean([]byte(rr.Body), "ok")
	if err != nil || !ok {
		return "", log, errors.Errorf("response not 'ok'")
	}

	// grab our message id
	externalID, err := jsonparser.GetInt([]byte(rr.Body), "result", "message_id")
	if err != nil {
		return "", log, errors.Errorf("no 'result.message_id' in response")
	}

	return strconv.FormatInt(externalID, 10), log, nil
}

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	confAuth := msg.Channel().ConfigForKey(courier.ConfigAuthToken, "")
	authToken, isStr := confAuth.(string)
	if !isStr || authToken == "" {
		return nil, fmt.Errorf("invalid auth token config")
	}

	// we only caption if there is only a single attachment
	caption := ""
	if len(msg.Attachments()) == 1 {
		caption = msg.Text()
	}

	// the status that will be written for this message
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)

	// whether we encountered any errors sending any parts
	hasError := true

	// figure out whether we have a keyboard to send as well
	qrs := msg.QuickReplies()
	replies := ""

	if len(qrs) > 0 {
		keys := make([]telegramKey, len(qrs))
		for i, qr := range qrs {
			keys[i].Text = qr
		}

		tk := telegramKeyboard{true, true, [][]telegramKey{keys}}
		replyBytes, err := json.Marshal(tk)
		if err != nil {
			return nil, err
		}
		replies = string(replyBytes)
	}

	// if we have text, send that if we aren't sending it as a caption
	if msg.Text() != "" && caption == "" {
		form := url.Values{
			"chat_id": []string{msg.URN().Path()},
			"text":    []string{msg.Text()},
		}

		externalID, log, err := h.sendMsgPart(msg, authToken, "sendMessage", form, replies)
		status.SetExternalID(externalID)
		hasError = err != nil
		status.AddLog(log)

		// clear our replies, they've been sent
		replies = ""
	}

	// send each attachment
	for _, attachment := range msg.Attachments() {
		mediaType, mediaURL := courier.SplitAttachment(attachment)
		switch strings.Split(mediaType, "/")[0] {
		case "image":
			form := url.Values{
				"chat_id": []string{msg.URN().Path()},
				"photo":   []string{mediaURL},
				"caption": []string{caption},
			}
			externalID, log, err := h.sendMsgPart(msg, authToken, "sendPhoto", form, replies)
			status.SetExternalID(externalID)
			hasError = err != nil
			status.AddLog(log)

		case "video":
			form := url.Values{
				"chat_id": []string{msg.URN().Path()},
				"video":   []string{mediaURL},
				"caption": []string{caption},
			}
			externalID, log, err := h.sendMsgPart(msg, authToken, "sendVideo", form, replies)
			status.SetExternalID(externalID)
			hasError = err != nil
			status.AddLog(log)

		case "audio":
			form := url.Values{
				"chat_id": []string{msg.URN().Path()},
				"audio":   []string{mediaURL},
				"caption": []string{caption},
			}
			externalID, log, err := h.sendMsgPart(msg, authToken, "sendAudio", form, replies)
			status.SetExternalID(externalID)
			hasError = err != nil
			status.AddLog(log)

		default:
			status.AddLog(courier.NewChannelLog("Unknown media type: "+mediaType, msg.Channel(), msg.ID(), "", "", courier.NilStatusCode,
				"", "", time.Duration(0), fmt.Errorf("unknown media type: %s", mediaType)))
			hasError = true

		}

		// clear our replies, we only send it on the first message
		replies = ""
	}

	if !hasError {
		status.SetStatus(courier.MsgWired)
	}

	return status, nil
}

var telegramAPIURL = "https://api.telegram.org"

func resolveFileID(channel courier.Channel, fileID string) (string, error) {
	confAuth := channel.ConfigForKey(courier.ConfigAuthToken, "")
	authToken, isStr := confAuth.(string)
	if !isStr || authToken == "" {
		return "", fmt.Errorf("invalid auth token config")
	}

	fileURL := fmt.Sprintf("%s/bot%s/getFile", telegramAPIURL, authToken)

	form := url.Values{}
	form.Set("file_id", fileID)

	req, err := http.NewRequest(http.MethodPost, fileURL, strings.NewReader(form.Encode()))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	if err != nil {
		return "", err
	}

	rr, err := utils.MakeHTTPRequest(req)
	if err != nil {
		return "", err
	}

	// was this request successful?
	ok, err := jsonparser.GetBoolean([]byte(rr.Body), "ok")
	if err != nil {
		return "", errors.Errorf("no 'ok' in response")
	}

	if !ok {
		return "", errors.Errorf("file id '%s' not present", fileID)
	}

	// grab the path for our file
	filePath, err := jsonparser.GetString([]byte(rr.Body), "result", "file_path")
	if err != nil {
		return "", errors.Errorf("no 'result.file_path' in response")
	}

	// return the URL
	return fmt.Sprintf("%s/file/bot%s/%s", telegramAPIURL, authToken, filePath), nil
}

type telegramKeyboard struct {
	ResizeKeyboard  bool            `json:"resize_keyboard"`
	OneTimeKeyboard bool            `json:"one_time_keyboard"`
	Keyboard        [][]telegramKey `json:"keyboard"`
}

type telegramKey struct {
	Text string `json:"text"`
}

type telegramFile struct {
	FileID   string `json:"file_id"    validate:"required"`
	FileSize int    `json:"file_size"`
}

type telegramLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// {
// 	"update_id": 174114370,
// 	"message": {
// 	  "message_id": 41,
//      "from": {
// 		  "id": 3527065,
// 		  "first_name": "Nic",
// 		  "last_name": "Pottier",
//        "username": "nicpottier"
// 	    },
//     "chat": {
//       "id": 3527065,
// 		 "first_name": "Nic",
//       "last_name": "Pottier",
//       "type": "private"
//     },
// 	   "date": 1454119029,
//     "text": "Hello World"
// 	 }
// }
type telegramEnvelope struct {
	UpdateID int64 `json:"update_id" validate:"required"`
	Message  struct {
		MessageID int64 `json:"message_id"`
		From      struct {
			ContactID int64  `json:"id"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Date    int64  `json:"date"`
		Text    string `json:"text"`
		Caption string `json:"caption"`
		Sticker *struct {
			Thumb telegramFile `json:"thumb"`
		} `json:"sticker"`
		Photo    []telegramFile    `json:"photo"`
		Video    *telegramFile     `json:"video"`
		Voice    *telegramFile     `json:"voice"`
		Document *telegramFile     `json:"document"`
		Location *telegramLocation `json:"location"`
		Venue    *struct {
			Location *telegramLocation `json:"location"`
			Title    string            `json:"title"`
			Address  string            `json:"address"`
		}
		Contact *struct {
			PhoneNumber string `json:"phone_number"`
			FirstName   string `json:"first_name"`
			LastName    string `json:"last_name"`
		}
	} `json:"message"`
}
