// Package mailer 负责发送邮件，移植自源仓库 server/src/mailer.ts。
//
// 邮件配置来源优先级：DB 中的默认/公司级配置 → 环境级主邮件（config.Mail）。
// 用 gopkg.in/gomail.v2 发信。包级单例保存「DB 配置读取器」，便于发验证码等场景复用。
package mailer

import (
	"context"
	"fmt"

	"gopkg.in/gomail.v2"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/types"
)

// Mailer 是可发信对象。
type Mailer struct {
	cfg *config.ServerConfig
	store db.Store
}

var (
	instance *Mailer
	once     struct{}
)

// NewMailer 构造邮件发送器（绑定配置与 store）。
func NewMailer(cfg *config.ServerConfig, store db.Store) *Mailer {
	return &Mailer{cfg: cfg, store: store}
}

// SetDefault 注册全局默认 mailer（main 启动时调用一次）。
func SetDefault(m *Mailer) { instance = m }

// Default 返回全局 mailer。
func Default() *Mailer { return instance }

// resolveConfig 解析本次发信用哪套配置：DB 默认配置优先，回退环境级主邮件。
func (m *Mailer) resolveConfig(ctx context.Context) (*types.MailConfig, error) {
	if m.store != nil {
		if c, err := m.store.GetDefaultMailConfig(ctx, db.MailConfigScope{}); err == nil && c != nil && c.Enabled {
			return c, nil
		}
	}
	if m.cfg != nil && m.cfg.Mail != nil && m.cfg.Mail.Enabled {
		sc := m.cfg.Mail
		return &types.MailConfig{
			ID: 0, Email: sc.User, DisplayName: sc.FromName, Host: sc.Host, Port: int64(sc.Port),
			Secure: sc.Secure, User: sc.User, Password: sc.Password, Enabled: true,
		}, nil
	}
	return nil, fmt.Errorf("no mail config")
}

// Send 发送一封邮件（to 收件人，subject 主题，html HTML 正文）。
func (m *Mailer) Send(ctx context.Context, to, subject, html string) error {
	c, err := m.resolveConfig(ctx)
	if err != nil {
		return err
	}
	from := c.Email
	if from == "" {
		from = c.User
	}
	display := c.DisplayName
	mg := gomail.NewMessage()
	mg.SetHeader("From", mg.FormatAddress(from, display))
	mg.SetHeader("To", to)
	mg.SetHeader("Subject", subject)
	mg.SetBody("text/html", html)
	d := gomail.NewDialer(c.Host, int(c.Port), c.User, c.Password)
	d.SSL = c.Secure
	if err := d.DialAndSend(mg); err != nil {
		logger.Error("mailer", "发信失败", "to", to, "host", c.Host)
		return err
	}
	logger.Info("mailer", "发信成功", "to", to, "subject", subject)
	return nil
}
