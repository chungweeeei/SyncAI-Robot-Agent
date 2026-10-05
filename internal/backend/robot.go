package backend

import "context"

// RobotState 對應 GET /api/v1/robot/state 的 JSON（backend 的 RobotState Pydantic model）。
// 只放 agent 用得到的欄位。這個 payload 是 backend 的白名單：
// motor_status.timestamp 不在裡面，所以從這裡看不出 driver 是不是中途掛掉。
type RobotState struct {
	// 秒（不是毫秒），整秒精度，1 Hz 更新。只用來判斷資料有沒有前進。
	Timestamp int64  `json:"timestamp"`
	RobotID   string `json:"robot_id"`
	Map       string `json:"map"`
	// 哪個 byobu session 在跑：MAINTENANCE / MANUAL / AUTO / UNKNOWN。
	// robot_state node 啟動後第一次 get_mode 回來之前固定是 AUTO。
	Mode         string `json:"mode"`
	LowLevelMode struct {
		// PPO / HIMLOCO / CHAMP / ISSAC / UNKNOWN
		Policy string `json:"policy"`
		// STAND / LOCOMOTION / LIE_DOWN / DAMPING / ESTOP / UNKNOWN。
		// driver 沒回報時也是 PPO/STAND，必須搭配 MotorStatus 是否為空來判斷。
		Motion string `json:"motion"`
	} `json:"low_level_mode"`
	// 只代表 map -> base_link 的 TF 存在，不代表定位品質。
	// false 時 LocalizationStatus 是全零的佔位值。
	LocalizationValid  bool `json:"localization_valid"`
	LocalizationStatus struct {
		Position struct {
			X     float64 `json:"x"`
			Y     float64 `json:"y"`
			Theta float64 `json:"theta"` // 度
		} `json:"position"`
		Velocity float64 `json:"velocity"`
	} `json:"localization_status"`
	NetworkStatus struct {
		RSSI int `json:"rssi"` // dBm；0 代表還沒收到 wifi 資料
	} `json:"network_status"`
	BatteryStatus struct {
		BatteryPercentage int `json:"battery_percentage"`
	} `json:"battery_status"`
	// 空陣列代表 syncai_driver_manager 沒在發布 motor_states。
	MotorStatus []struct {
		Name        string `json:"name"`
		Temperature int    `json:"temperature"` // °C
		Error       int    `json:"error"`       // 0 代表正常
	} `json:"motor_status"`
}

// GetRobotState 讀取 backend 快取的最後一筆機器人狀態。
// 注意：backend 只保存最後一筆，node 掛掉後仍會一直回 200，內容停在最後一筆。
func (c *Client) GetRobotState(ctx context.Context) (RobotState, error) {
	var st RobotState
	err := c.getJSON(ctx, "/api/v1/robot/state", &st)
	return st, err
}
