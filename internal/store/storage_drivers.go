package store

import "pimg/internal/storage"

type DriverField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Hint     string `json:"hint,omitempty"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}
type DriverDefinition struct {
	ID     string                                                                                                     `json:"id"`
	Name   string                                                                                                     `json:"name"`
	Fields []DriverField                                                                                              `json:"fields"`
	Open   func(*Service, string, InstanceConfig) (storage.Cold, storage.Hot, *storage.LocalHot, SourceDriver, error) `json:"-"`
}
type ServiceOption func(*Service)

// WithStorageDriver registers an adapter before persisted instances are loaded.
// The same definition is supplied on restart; the UI uses its field description.
func WithStorageDriver(d DriverDefinition) ServiceOption {
	return func(s *Service) { s.extraDrivers = append(s.extraDrivers, d) }
}
func (s *Service) StorageDrivers() []DriverDefinition {
	out := []DriverDefinition{
		{ID: "local", Name: "本地目录", Fields: []DriverField{{Key: "directory", Label: "存储文件夹", Hint: "新存储自动管理 archives、display；旧存储沿用原位置，不搬动文件。Docker 请填容器内路径。", Type: "text", Required: true}}},
		{ID: "telegram", Name: "Telegram 归档", Fields: []DriverField{}},
		{ID: "r2", Name: "Cloudflare R2", Fields: []DriverField{{Key: "endpoint", Label: "R2 端点", Type: "url", Required: true}, {Key: "bucket", Label: "存储桶", Type: "text", Required: true}, {Key: "prefix", Label: "实例前缀", Hint: "同一桶内使用互不重叠的前缀。", Type: "text"}, {Key: "public_base", Label: "公开域名", Hint: "永久发布需填写公开域名；留空可作归档和临时存储。", Type: "url"}, {Key: "access_key", Label: "Access Key ID", Type: "password", Required: true}, {Key: "secret_key", Label: "Secret Access Key", Hint: "保存后不回显。", Type: "password", Required: true}}},
	}
	return append(out, s.extraDrivers...)
}
