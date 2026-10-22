package store

import (
	"context"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

func TestNewGroupTitleUsesAccountUsernameAndPreservesExistingGroup(t *testing.T) {
	s, transport := workflowEnv(t)
	if err := s.DB.EnsureAdmin("站长", "hash"); err != nil {
		t.Fatal(err)
	}
	if title, err := s.accountGroupTitle(dbx.AdminOwner); err != nil || title != "站长" {
		t.Fatal("admin default is not its username", title, err)
	}
	fake := &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}}
	s.UserTG = fake
	owner := "user:group-default"
	if err := s.DB.CreateAccount(owner, "家庭照片", "hash"); err != nil {
		t.Fatal(err)
	}
	space, err := s.EnsureTelegramSpace(context.Background(), owner)
	if err != nil || space.ChatTitle != "家庭照片" || fake.calls != 1 {
		t.Fatal("new group title is not its owner's username", space, err)
	}
	if _, err := s.DB.Exec(`UPDATE account_telegram SET chat_title='自定义群名' WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE accounts SET username='新用户名' WHERE id=?`, owner); err != nil {
		t.Fatal(err)
	}
	current, err := s.EnsureTelegramSpace(context.Background(), owner)
	if err != nil || current.ChatTitle != "自定义群名" || current.ChatID != space.ChatID || fake.calls != 1 {
		t.Fatal("existing group was renamed or recreated", current, err)
	}
}

type titleTG struct {
	*provisionTG
	calls  int
	title  string
	err    error
	during func()
}

func (f *titleTG) RenameGroup(_ context.Context, chat int64, title string) error {
	f.calls++
	if f.during != nil {
		fn := f.during
		f.during = nil
		fn()
	}
	if f.err != nil {
		return f.err
	}
	f.title = title
	return nil
}
func titleFixture(t *testing.T) (*Service, *titleTG, string) {
	t.Helper()
	s, transport := workflowEnv(t)
	f := &titleTG{provisionTG: &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}}}
	s.UserTG = f
	owner := "user:title"
	if err := s.DB.CreateAccount(owner, "家庭相册", "hash"); err != nil {
		t.Fatal(err)
	}
	space, err := s.EnsureTelegramSpace(context.Background(), owner)
	if err != nil || space.ChatTitle != "家庭相册" {
		t.Fatal(space, err)
	}
	return s, f, owner
}

func TestGroupTitleLatestEditWinsWithoutChangingBinding(t *testing.T) {
	s, f, owner := titleFixture(t)
	before, _ := s.DB.TelegramSpace(owner)
	s.QueueAccountGroupTitle(owner, "旧名称")
	s.QueueAccountGroupTitle(owner, "新名称")
	if err := s.RunAccountGroupTitle(context.Background(), owner, "旧名称"); err != nil || f.calls != 0 {
		t.Fatal("superseded edit ran", err)
	}
	f.during = func() {
		if _, err := s.QueueAccountGroupTitle(owner, "最终名称"); err != nil {
			t.Error(err)
		}
	}
	if err := s.RunAccountGroupTitle(context.Background(), owner, "新名称"); err != nil {
		t.Fatal(err)
	}
	middle, _ := s.DB.TelegramSpace(owner)
	if middle.DesiredTitle != "最终名称" {
		t.Fatal("lost edit arriving during RPC", middle)
	}
	if err := s.RunAccountGroupTitle(context.Background(), owner, "最终名称"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.DB.TelegramSpace(owner)
	if after.ChatID != before.ChatID || after.Token != before.Token || after.ChatTitle != "最终名称" || after.DesiredTitle != "" || f.calls != 2 {
		t.Fatal(after, f.calls)
	}
}

func TestGroupTitleRetryKeepsProviderCooldownAndCommittedGroup(t *testing.T) {
	s, f, owner := titleFixture(t)
	before, _ := s.DB.TelegramSpace(owner)
	f.err = &tguser.RetryAfterError{Wait: 2 * time.Hour}
	s.QueueAccountGroupTitle(owner, "我的备份")
	if err := s.RunAccountGroupTitle(context.Background(), owner, "我的备份"); err == nil {
		t.Fatal("failed rename accepted")
	}
	b, _ := s.DB.TelegramSpace(owner)
	if b.TitleRetryAfter < time.Now().Add(119*time.Minute).Unix() || b.ChatID != before.ChatID || b.State != "ready" {
		t.Fatal(b)
	}
	s.QueueAccountGroupTitle(owner, "更新后的备份")
	if err := s.RunAccountGroupTitle(context.Background(), owner, "更新后的备份"); err == nil || f.calls != 1 {
		t.Fatal("manual edit bypassed provider wait", err, f.calls)
	}
	s.DB.Exec(`UPDATE account_telegram SET title_retry_after=0 WHERE owner_id=?`, owner)
	f.err = nil
	if err := s.RunAccountGroupTitle(context.Background(), owner, "更新后的备份"); err != nil {
		t.Fatal(err)
	}
	if b, _ = s.DB.TelegramSpace(owner); b.ChatTitle != "更新后的备份" || b.TitleError != "" || b.ChatID != before.ChatID || f.provisionTG.calls != 1 {
		t.Fatal("rename retry changed group", b)
	}
	if err := s.RunAccountGroupTitle(context.Background(), "missing", "name"); err == nil {
		t.Fatal("missing account group accepted")
	}
}
