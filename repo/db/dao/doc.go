// Package dao 数据访问对象层：各域模型的 GORM 持久化实现。
//
// # DB 注入约定（去全局化终态）
//
// 全部 Repository 构造函数必填 *gorm.DB：
//
//	repo := dao.NewFileCodeRepository(gormDB) // 必填注入，全部读写落在 gormDB 实例
//
// 规则：
//   - 无全局回退：db() 只返回构造注入的 conn，不存在隐式全局 DB 状态；
//   - app/ 与本包一律不得调用 repo/db.GetDB/SetDatabaseInstance；
//   - repo/db.GetDB() 仅是装配层显式句柄（bootstrap 初始化结果的出口，fnos/qnap
//     SSO 等库式消费方显式取用），调用点须带「装配层显式句柄」注释；
//   - 动机：解锁不依赖全局 DB 状态的隔离测试（:memory: 库、并行子测试），
//     以及嵌入式/多实例场景（同进程多套库）。
package dao
