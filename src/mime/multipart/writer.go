// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package multipart

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strings"
)

// A Writer generates multipart messages.
type Writer struct {
	w        io.Writer
	boundary string
	lastpart *part
}

// NewWriter returns a new multipart Writer with a random boundary,
// writing to w.
func NewWriter(w io.Writer) *Writer { // 创建一个新的multipart的writer
	return &Writer{
		w:        w,
		boundary: randomBoundary(),
	}
}

// Boundary returns the Writer's boundary.
func (w *Writer) Boundary() string { // 返回writer的boundary
	return w.boundary
}

// SetBoundary overrides the Writer's default randomly-generated
// boundary separator with an explicit value.
//
// SetBoundary must be called before any parts are created, may only
// contain certain ASCII characters, and must be non-empty and
// at most 69 bytes long.
func (w *Writer) SetBoundary(boundary string) error { // 覆盖Writer缺省的随机生成的boundary
	if w.lastpart != nil { // 不能先写之后再调用SetBoundary
		return errors.New("mime: SetBoundary called after write")
	}
	// rfc2046#section-5.1.1
	if len(boundary) < 1 || len(boundary) > 69 { // boundary长度在1到69之间
		return errors.New("mime: invalid boundary length")
	}
	for _, b := range boundary { // 校验Boundary中的有效字符
		if 'A' <= b && b <= 'Z' || 'a' <= b && b <= 'z' || '0' <= b && b <= '9' {
			continue
		}
		switch b {
		case '\'', '(', ')', '+', '_', ',', '-', '.', '/', ':', '=', '?':
			continue
		}
		return errors.New("mime: invalid boundary character")
	}
	w.boundary = boundary
	return nil
}

// FormDataContentType returns the Content-Type for an HTTP
// multipart/form-data with this Writer's Boundary.
func (w *Writer) FormDataContentType() string { // 返回Content-Type类型，因为要加入boundary
	return "multipart/form-data; boundary=" + w.boundary
}

func randomBoundary() string { // 返回一个随机的Boundary的串
	var buf [30]byte
	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", buf[:])
}

// CreatePart creates a new multipart section with the provided
// header. The body of the part should be written to the returned
// Writer. After calling CreatePart, any previous part may no longer
// be written to.
func (w *Writer) CreatePart(header textproto.MIMEHeader) (io.Writer, error) { // 根据header创建一个新part
	if w.lastpart != nil {
		if err := w.lastpart.close(); err != nil {
			return nil, err
		}
	}
	var b bytes.Buffer
	if w.lastpart != nil { // 根据是否有上一个part设置是否加一个\r\n
		fmt.Fprintf(&b, "\r\n--%s\r\n", w.boundary)
	} else {
		fmt.Fprintf(&b, "--%s\r\n", w.boundary)
	}
	// TODO(bradfitz): move this to textproto.MimeHeader.Write(w), have it sort
	// and clean, like http.Header.Write(w) does.
	for k, vv := range header { // 遍历所有的MIMEHeader
		for _, v := range vv {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v) // 写Header信息
		}
	}
	fmt.Fprintf(&b, "\r\n")    // 回车换行作为Header结束
	_, err := io.Copy(w.w, &b) // 将buffer的内容写入w包含的Writer中
	if err != nil {
		return nil, err
	}
	p := &part{
		mw: w,
	}
	w.lastpart = p
	return p, nil
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}

// CreateFormFile is a convenience wrapper around CreatePart. It creates
// a new form-data header with the provided field name and file name.
func (w *Writer) CreateFormFile(fieldname, filename string) (io.Writer, error) { // 设置域名和文件名
	h := make(textproto.MIMEHeader) // 创建MIMEHeader结构
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
			escapeQuotes(fieldname), escapeQuotes(filename)))
	h.Set("Content-Type", "application/octet-stream")
	return w.CreatePart(h)
}

// CreateFormField calls CreatePart with a header using the
// given field name.
func (w *Writer) CreateFormField(fieldname string) (io.Writer, error) { // 创建part设置域名
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="%s"`, escapeQuotes(fieldname)))
	return w.CreatePart(h)
}

// WriteField calls CreateFormField and then writes the given value.
func (w *Writer) WriteField(fieldname, value string) error { // 写入域名和值
	p, err := w.CreateFormField(fieldname)
	if err != nil {
		return err
	}
	_, err = p.Write([]byte(value))
	return err
}

// Close finishes the multipart message and writes the trailing
// boundary end line to the output.
func (w *Writer) Close() error {
	if w.lastpart != nil {
		if err := w.lastpart.close(); err != nil {
			return err
		}
		w.lastpart = nil
	}
	_, err := fmt.Fprintf(w.w, "\r\n--%s--\r\n", w.boundary)
	return err
}

type part struct {
	mw     *Writer
	closed bool
	we     error // last error that occurred writing
}

func (p *part) close() error { // 关闭一个part
	p.closed = true
	return p.we
}

func (p *part) Write(d []byte) (n int, err error) { // 向一个Part中写数据
	if p.closed {
		return 0, errors.New("multipart: can't write to finished part")
	}
	n, err = p.mw.w.Write(d)
	if err != nil {
		p.we = err
	}
	return
}
