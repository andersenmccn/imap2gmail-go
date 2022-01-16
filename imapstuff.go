package main

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/emersion/go-imap"
	idle "github.com/emersion/go-imap-idle"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/charset"
)

func imapGetClient() (*client.Client, error) {
	log.Println("imapGetClient: connecting...")

	imap.CharsetReader = charset.Reader

	c, err := client.DialTLS(Conf.ConfImap.Server+":"+strconv.Itoa(Conf.ConfImap.Port), nil)
	if err != nil {
		return nil, errors.New("imapGetClient dial: " + err.Error())
	}
	// c.SetDebug(os.Stdout) // enable for debug
	c.Timeout = 2 * Conf.ConfImap.IdleTimeout

	if err := c.Login(Conf.ConfImap.User, Conf.ConfImap.Password); err != nil {
		return nil, errors.New("imapGetClient login: " + err.Error())
	}
	log.Println("   Logged in")
	return c, nil
}

// ImapTest xxx
func ImapTest() error {

	c, err := imapGetClient()
	if err != nil {
		return errors.New("imapTest: " + err.Error())
	}

	// List mailboxes
	mailboxes := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", "*", mailboxes)
	}()

	for m := range mailboxes {
		log.Println("ImapTest: Mailbox " + m.Name)
	}

	if err := <-done; err != nil {
		return errors.New("imapTest: " + err.Error())
	}
	return nil
}

func imapMoveTo(c *client.Client, seqset *imap.SeqSet, targetFolder string) error {
	// copy to moved folder
	log.Printf("imapMoveTo: copy to folder %v...", targetFolder)
	if err := c.Copy(seqset, targetFolder); err != nil {
		return err
	}

	// delete inbox msg
	log.Println("imapMoveTo: mark deleted...")
	if err := c.Store(seqset, imap.FormatFlagsOp(imap.AddFlags, false), []interface{}{imap.DeletedFlag}, nil); err != nil {
		return err
	}
	log.Println("imapMoveTo: expunge...")
	if err := c.Expunge(nil); err != nil {
		return err
	}
	return nil
}

func imapHandleFirstMsg(mId uint32, c *client.Client, mbox *imap.MailboxStatus) error {
	log.Println("handlefirst: begin")

	seqset := new(imap.SeqSet)
	seqset.AddNum(mId) // newest message first
	log.Printf("handlefirst: seqset: %v", seqset)

	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchRFC822Size}
	log.Printf("handlefirst: items: %v", items)

	messages := make(chan *imap.Message, 1)
	log.Printf("handlefirst: messages: %v", messages)

	// done := make(chan error, 1)

	log.Println("handlefirst: get first message envelope & size")
	go func() {
		if err := c.Fetch(seqset, items, messages); err != nil {
			log.Fatalf("handlefirst: fetch messages error: %v", err)
		}
	}()

	msg := <-messages
	log.Printf("handlefirst: msg: %v", msg)

	if msg == nil {
		return errors.New("Couldn't get first message envelope & size")
	}
	mSubject := msg.Envelope.Subject
	mID := msg.Envelope.MessageId
	mSize := msg.Size // note that at least for exchange servers, this is useless orig file size(s), NOT b64 encoded. joke.
	log.Printf("handlefirst:   size=%v, id=%v, subject=%v", mSize, mID, mSubject)
	if mSize > uint32(Conf.ConfImap.MaxEmailSize) {
		log.Printf("handlefirst: message too big: %v, moving to quarantine...", mSize)
		SendEmail(fmt.Sprintf("Moving message to quarantine mid=%v", mID), fmt.Sprintf("Message too big: %v\nsubject=%v", mSize, mSubject))
		return imapMoveTo(c, seqset, Conf.ConfImap.FolderQuarantine)
	}

	log.Println("handlefirst: get first message raw")
	messages = make(chan *imap.Message, 1) // channels need to be reopened
	section := &imap.BodySectionName{}
	go func() {
		if err := c.Fetch(seqset, []imap.FetchItem{section.FetchItem()}, messages); err != nil {
			log.Fatalf("handlefirst: fetch body error: %v", err)
		}
	}()
	msg = <-messages
	if msg == nil {
		return errors.New("Couldn't get first message raw")
	}
	r := msg.GetBody(section)
	if r == nil {
		return errors.New("Server didn't returned message body")
	}

	origlen := r.Len()

	log.Printf("handlefirst: read raw... (origlen=%v)", origlen)
	p := make([]byte, origlen)
	rlen, err := r.Read(p)
	if err != nil {
		return err
	}
	if origlen != rlen {
		return fmt.Errorf("read wrong: origlen=%v and rlen=%v", origlen, rlen)
	}

	log.Println("handlefirst: import into gmail...")
	if err := GmailImport(string(p)); err != nil {
		log.Println("handlefirst: GmailImport gave error, moving to quarantine: ", err)
		SendEmail(fmt.Sprintf("Moving message to quarantine mid=%v", mID), fmt.Sprintf("GmailImport error: %v\nsubject=%v", err, mSubject))
		return imapMoveTo(c, seqset, Conf.ConfImap.FolderQuarantine)
	}

	log.Println("handlefirst: move to moved folder...")
	if err := imapMoveTo(c, seqset, Conf.ConfImap.FolderMoved); err != nil {
		return err
	}

	log.Println("handlefirst: done!")
	return nil
}

func imapIdleWait(c *client.Client, mbox *imap.MailboxStatus) error {
	idleClient := idle.NewClient(c)

	chChUpdates := make(chan client.Update)
	c.Updates = chChUpdates
	defer func() { c.Updates = nil }() // important, before return release this otherwise imap hangs!

	chStopIdle := make(chan struct{})
	chIdleres := make(chan error, 1)

	// run idle
	go func() {
		chIdleres <- idleClient.IdleWithFallback(chStopIdle, 0)
	}()

	log.Println("wait for idle events, msgcount: ", mbox.Messages)
	for {
		select {
		case update := <-chChUpdates: // stop idle if update
			log.Printf("idle update: %v (type %T), msgcount %v", update, update, mbox.Messages)
			go func() { chStopIdle <- struct{}{} }()
		case <-time.After(Conf.ConfImap.IdleTimeout): // stop idle after custom timeout
			log.Printf("idle stop by user timeout!")
			go func() { chStopIdle <- struct{}{} }()
		case err := <-chIdleres: // this is called after chStopIdle is notified or idle error
			log.Printf("idle done, res=%v", err)
			if err != nil {
				return err
			}
			return nil
		}
	}
}

// ImapLoop is the main loop. It returns if error or imap conn broken.
func ImapLoop(wdog chan error) (errres error) {

	// catch all panics
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Recovering from panic in ImapLoop, r=%v \n", r)
			errres = fmt.Errorf("ImapLoop: recovered panic: %v", r)
		}
	}()

	c, err := imapGetClient()
	if err != nil {
		return err
	}

	// Select INBOX
	mbox, err := c.Select(Conf.ConfImap.Folder, false)
	if err != nil {
		return err
	}

	// 搜索条件实例对象
	criteria := imap.NewSearchCriteria()

	// ALL是默认条件
	// See RFC 3501 section 6.4.4 for a list of searching criteria.
	criteria.WithoutFlags = []string{"ALL"}
	ids, _ := c.Search(criteria)

	for {
		log.Printf("imaploop: have %v messages", mbox.Messages)

		// move existing messages to gmail
		for mid := range ids {
			log.Printf("imaploop: handle first message...")
			if err := imapHandleFirstMsg(uint32(mid), c, mbox); err != nil {
				log.Printf("imaploop: handle error: %v", err)
				// return err
			}
			log.Printf("imaploop: import of message %d done!", mid)
		}

		// wait clever
		log.Println("imaploop: before imap idle wait...")
		if err := imapIdleWait(c, mbox); err != nil {
			return err
		}

		wdog <- nil // if no errors, silence watchdog

		log.Println("imaploop: after imap idle wait")
	}
}

func SimpleUsage() {
	// 连接邮件服务器
	c, err := imapGetClient()
	if err != nil {
		log.Fatal(err)
	}
	// Don't forget to logout
	defer c.Logout()

	log.Println("SELECT")
	// 选择收件箱
	mbox, err := c.Select("INBOX", false)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("FROM")
	// 获取近50封邮件
	from := uint32(50)
	to := mbox.Messages
	if mbox.Messages > 50 {
		// We're using unsigned integers here, only subtract if the result is > 0
		from = mbox.Messages - 50
	}
	seqset := new(imap.SeqSet)
	// 设置邮件搜索范围
	seqset.AddRange(from, to)

	log.Printf("SEQSET %v", seqset)

	messages := make(chan *imap.Message, 10)

	log.Printf("MSG INIT %v", messages)
	done := make(chan error, 1)
	go func() {
		// 抓取邮件消息体传入到messages信道
		done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchRFC822}, messages)
	}()

	log.Printf("messages %v", len(messages))

	for msg := range messages {
		if msg != nil {
			// 打印邮件标题
			log.Println("* " + msg.Envelope.Subject)
		}
	}

	if err = <-done; err != nil {
		log.Fatal(err)
	}
}

func pop(list *[]uint32) uint32 {
	length := len(*list)
	lastEle := (*list)[length-1]
	*list = (*list)[:length-1]
	return lastEle
}

func Usage() {
	// 连接邮件服务器
	c, err := imapGetClient()
	if err != nil {
		log.Fatal(err)
	}
	// Don't forget to logout
	defer c.Logout()

	// 选择收件箱
	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Fatal(err)
	}

	// 搜索条件实例对象
	criteria := imap.NewSearchCriteria()

	// ALL是默认条件
	// See RFC 3501 section 6.4.4 for a list of searching criteria.
	criteria.WithoutFlags = []string{"ALL"}
	ids, _ := c.Search(criteria)

	for {
		if len(ids) == 0 {
			break
		}
		id := pop(&ids)

		seqset := new(imap.SeqSet)
		seqset.AddNum(id)
		chanMessage := make(chan *imap.Message, 1)
		go func() {
			// 第一次fetch, 只抓取邮件头，邮件标志，邮件大小等信息，执行速度快
			if err = c.Fetch(seqset,
				[]imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchRFC822Size},
				chanMessage); err != nil {
				// 【实践经验】这里遇到过的err信息是：ENVELOPE doesn't contain 10 fields
				// 原因是对方发送的邮件格式不规范，解析失败
				// 相关的issue: https://github.com/emersion/go-imap/issues/143
				log.Println(seqset, err)
			}
		}()

		message := <-chanMessage
		if message == nil {
			log.Println("Server didn't returned message")
			continue
		}
		fmt.Printf("%v: %v bytes, flags=%v \n", message.SeqNum, message.Size, message.Flags)

		log.Println(message.Envelope.Subject)

		// var s imap.BodySectionName
		// if strings.HasPrefix(message.Envelope.Subject, "subject") {
		// 	chanMsg := make(chan *imap.Message, 1)
		// 	go func() {
		// 		// 这里是第二次fetch, 获取邮件MIME内容
		// 		if err = c.Fetch(seqset, []imap.FetchItem{imap.FetchRFC822}, chanMsg); err != nil {
		// 			log.Println(seqset, err)
		// 		}
		// 	}()

		// 	msg := <-chanMsg
		// 	if msg == nil {
		// 		log.Println("Server didn't returned message")
		// 	}

		// 	log.Println(msg.Envelope.Subject)

		// }
	}
}
