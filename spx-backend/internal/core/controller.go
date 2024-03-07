package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	_ "image/png"
	"io/ioutil"
	"mime/multipart"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goplus/builder/spx-backend/internal/common"

	_ "github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
	_ "github.com/qiniu/go-cdk-driver/kodoblob"
	"gocloud.dev/blob"
	"golang.org/x/tools/imports"
)

var (
	ErrNotExist = os.ErrNotExist
)

type Config struct {
	Driver string // database driver. default is `mysql`.
	DSN    string // database data source name
	BlobUS string // blob URL scheme
}

type AssetAddressData struct {
	Assets    map[string]string `json:"assets"`
	IndexJson string            `json:"indexJson"`
	Type      string            `json:"type"`
	Url       string            `json:"url"`
}

type Asset struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AuthorId   string    `json:"authorId"`
	Category   string    `json:"category"`
	IsPublic   int       `json:"isPublic"`  // 1:Public state 0: Personal state
	Address    string    `json:"address"`   // The partial path of the asset's location, excluding the host. like 'sprite/xxx.svg'
	AssetType  string    `json:"assetType"` // 0：sprite，1：background，2：sound
	ClickCount string    `json:"clickCount"`
	Status     int       `json:"status"` // 1:Normal state 0:Deleted status
	CTime      time.Time `json:"cTime"`
	UTime      time.Time `json:"uTime"`
}

type Project struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	AuthorId string    `json:"authorId"`
	Address  string    `json:"address"`
	IsPublic int       `json:"isPublic"`
	Status   int       `json:"status"`
	Version  int       `json:"version"`
	Ctime    time.Time `json:"cTime"`
	Utime    time.Time `json:"uTime"`
}

type Controller struct {
	bucket *blob.Bucket
	db     *sql.DB
}

type FormatError struct {
	Column int
	Line   int
	Msg    string
}
type FormatResponse struct {
	Body  string
	Error FormatError
}

// New init Config
func New(ctx context.Context, conf *Config) (ret *Controller, err error) {
	err = godotenv.Load("../.env")
	if err != nil {
		println(err.Error())
		return
	}
	if conf == nil {
		conf = new(Config)
	}
	driver := conf.Driver
	dsn := conf.DSN
	bus := conf.BlobUS
	if driver == "" {
		driver = "mysql"
	}
	if dsn == "" {
		dsn = os.Getenv("GOP_SPX_DSN")
	}
	if bus == "" {
		bus = os.Getenv("GOP_SPX_BLOBUS")
	}
	println(bus)
	println(dsn)
	bucket, err := blob.OpenBucket(ctx, bus)
	if err != nil {
		println(err.Error())
		return
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		println(err.Error())
		return
	}
	return &Controller{bucket, db}, nil
}

// ProjectInfo Find project from db
func (ctrl *Controller) ProjectInfo(ctx context.Context, id string) (*Project, error) {
	if id != "" {
		pro, err := common.QueryById[Project](ctrl.db, id)
		if err != nil {
			return nil, err
		}
		if pro == nil {
			return nil, err
		}
		pro.Address = os.Getenv("QINIU_PATH") + "/" + pro.Address
		return pro, nil
	}
	return nil, ErrNotExist
}

// DeleteProject Delete Project
func (ctrl *Controller) DeleteProject(ctx context.Context, id string, userId string) error {

	project, err := common.QueryById[Project](ctrl.db, id)
	if project.AuthorId != userId {
		return common.ErrPermissions
	}
	err = ctrl.bucket.Delete(ctx, project.Address)
	if err != nil {
		return err
	}
	return DeleteProjectById(ctrl.db, id)

}

// SaveProject Save project
func (ctrl *Controller) SaveProject(ctx context.Context, project *Project, file multipart.File, header *multipart.FileHeader) (*Project, error) {
	if project.ID == "" {
		path, err := UploadFile(ctx, ctrl, os.Getenv("PROJECT_PATH"), file, header.Filename)
		if err != nil {
			return nil, err
		}
		project.Address = path
		project.Version = 1
		project.Status = 1
		project.IsPublic = common.PERSONAL
		project.ID, err = AddProject(ctrl.db, project)
		return project, err
	} else {
		address := GetProjectAddress(project.ID, ctrl.db)
		version := GetProjectVersion(project.ID, ctrl.db)
		err := ctrl.bucket.Delete(ctx, address)
		if err != nil {
			return nil, err
		}
		path, err := UploadFile(ctx, ctrl, os.Getenv("PROJECT_PATH"), file, header.Filename)
		if err != nil {
			return nil, err
		}
		project.Address = path
		project.Version = version + 1
		return project, UpdateProject(ctrl.db, project)
	}
}

// CodeFmt Format code
func (ctrl *Controller) CodeFmt(ctx context.Context, body, fiximport string) (res *FormatResponse) {

	fs, err := splitFiles([]byte(body))
	if err != nil {
		fmtErr := ExtractErrorInfo(err.Error())
		res = &FormatResponse{
			Body:  "",
			Error: fmtErr,
		}
		return
	}
	fixImports := fiximport != ""
	for _, f := range fs.files {
		switch {
		case path.Ext(f) == ".go":
			var out []byte
			var err error
			in := fs.Data(f)
			if fixImports {
				// TODO: pass options to imports.Process so it
				// can find symbols in sibling files.
				out, err = imports.Process(f, in, nil)
			} else {
				var tmpDir string
				tmpDir, err = os.MkdirTemp("", "gopformat")
				if err != nil {
					fmtErr := ExtractErrorInfo(err.Error())
					res = &FormatResponse{
						Body:  "",
						Error: fmtErr,
					}
					return
				}
				defer os.RemoveAll(tmpDir)
				tmpGopFile := filepath.Join(tmpDir, "prog.gop")
				if err = os.WriteFile(tmpGopFile, in, 0644); err != nil {
					fmtErr := ExtractErrorInfo(err.Error())
					res = &FormatResponse{
						Body:  "",
						Error: fmtErr,
					}
					return
				}
				cmd := exec.Command("gop", "fmt", "-smart", tmpGopFile)
				//gop fmt returns error result in stdout, so we do not need to handle stderr
				//err is to check gop fmt return code
				var fmtErr []byte
				fmtErr, err = cmd.Output()
				if err != nil {
					fmtErr := ExtractErrorInfo(strings.Replace(string(fmtErr), tmpGopFile, "prog.gop", -1))
					res = &FormatResponse{
						Body:  "",
						Error: fmtErr,
					}
					return
				}
				out, err = ioutil.ReadFile(tmpGopFile)
				if err != nil {
					err = errors.New("interval error when formatting gop code")
				}
			}
			if err != nil {
				errMsg := err.Error()
				if !fixImports {
					// Unlike imports.Process, format.Source does not prefix
					// the error with the file path. So, do it ourselves here.
					errMsg = fmt.Sprintf("%v:%v", f, errMsg)
				}
				fmtErr := ExtractErrorInfo(errMsg)
				res = &FormatResponse{
					Body:  "",
					Error: fmtErr,
				}
				return
			}
			fs.AddFile(f, out)
		case path.Base(f) == "go.mod":
			out, err := common.FormatGoMod(f, fs.Data(f))
			if err != nil {
				fmtErr := ExtractErrorInfo(err.Error())
				res = &FormatResponse{
					Body:  "",
					Error: fmtErr,
				}
				return
			}
			fs.AddFile(f, out)
		}
	}
	res = &FormatResponse{
		Body:  string(fs.Format()),
		Error: FormatError{},
	}
	return
}

// Asset returns an Asset.
func (ctrl *Controller) Asset(ctx context.Context, id string) (*Asset, error) {
	asset, err := common.QueryById[Asset](ctrl.db, id)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, err
	}
	modifiedAddress, err := ctrl.ModifyAssetAddress(asset.Address)
	if err != nil {
		return nil, err
	}
	asset.Address = modifiedAddress

	return asset, nil
}

// AssetPubList list public assets
func (ctrl *Controller) AssetPubList(ctx context.Context, pageIndex string, pageSize string, assetType string, category string, isOrderByTime string, isOrderByHot string) (*common.Pagination[Asset], error) {
	wheres := []common.FilterCondition{
		{Column: "asset_type", Operation: "=", Value: assetType},
	}

	wheres = append(wheres, common.FilterCondition{Column: "is_public", Operation: "=", Value: common.PUBLIC})
	var orders []common.OrderByCondition
	if category != "" {
		wheres = append(wheres, common.FilterCondition{Column: "category", Operation: "=", Value: category})
	}
	if isOrderByTime != "" {
		orders = append(orders, common.OrderByCondition{Column: "c_time", Direction: "desc"})
	}
	if isOrderByHot != "" {
		orders = append(orders, common.OrderByCondition{Column: "click_count", Direction: "desc"})
	}
	pagination, err := common.QueryByPage[Asset](ctrl.db, pageIndex, pageSize, wheres, orders)
	for i, asset := range pagination.Data {
		modifiedAddress, err := ctrl.ModifyAssetAddress(asset.Address)
		if err != nil {
			return nil, err
		}
		pagination.Data[i].Address = modifiedAddress
	}
	if err != nil {
		return nil, err
	}
	return pagination, nil
}

// UserAssetList list personal assets
func (ctrl *Controller) UserAssetList(ctx context.Context, pageIndex string, pageSize string, assetType string, category string, isOrderByTime string, isOrderByHot string, uid string) (*common.Pagination[Asset], error) {
	wheres := []common.FilterCondition{
		{Column: "asset_type", Operation: "=", Value: assetType},
	}
	var orders []common.OrderByCondition
	if category != "" {
		wheres = append(wheres, common.FilterCondition{Column: "category", Operation: "=", Value: category})
	}
	wheres = append(wheres, common.FilterCondition{Column: "author_id", Operation: "=", Value: uid})
	if isOrderByTime != "" {
		orders = append(orders, common.OrderByCondition{Column: "c_time", Direction: "desc"})
	}
	if isOrderByHot != "" {
		orders = append(orders, common.OrderByCondition{Column: "click_count", Direction: "desc"})
	}
	pagination, err := common.QueryByPage[Asset](ctrl.db, pageIndex, pageSize, wheres, orders)
	for i, asset := range pagination.Data {
		modifiedAddress, err := ctrl.ModifyAssetAddress(asset.Address)
		if err != nil {
			return nil, err
		}
		pagination.Data[i].Address = modifiedAddress
	}
	if err != nil {
		return nil, err
	}
	return pagination, nil
}

// IncrementAssetClickCount increments the click count for an asset.
func (ctrl *Controller) IncrementAssetClickCount(ctx context.Context, id string, assetType string) error {
	query := "UPDATE asset SET click_count = click_count + 1 WHERE id = ? and asset_type = ?"
	_, err := ctrl.db.ExecContext(ctx, query, id, assetType)
	if err != nil {
		return err
	}
	return nil
}

// ModifyAssetAddress transfers relative path to download url
func (ctrl *Controller) ModifyAssetAddress(address string) (string, error) {
	var data AssetAddressData
	if err := json.Unmarshal([]byte(address), &data); err != nil {
		return "", err
	}
	qiniuPath := os.Getenv("QINIU_PATH")
	for key, value := range data.Assets {
		data.Assets[key] = qiniuPath + "/" + value
	}
	if data.IndexJson != "" {
		data.IndexJson = qiniuPath + "/" + data.IndexJson
	}
	if data.Url != "" {
		data.Url = qiniuPath + "/" + data.Url
	}
	modifiedAddress, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return string(modifiedAddress), nil
}

// PubProjectList Public project list
func (ctrl *Controller) PubProjectList(ctx context.Context, pageIndex string, pageSize string) (*common.Pagination[Project], error) {
	wheres := []common.FilterCondition{
		{Column: "is_public", Operation: "=", Value: common.PUBLIC},
	}
	pagination, err := common.QueryByPage[Project](ctrl.db, pageIndex, pageSize, wheres, nil)
	if err != nil {
		return nil, err
	}
	for i := range pagination.Data {
		pagination.Data[i].Address = os.Getenv("QINIU_PATH") + "/" + pagination.Data[i].Address
	}
	return pagination, nil
}

// UserProjectList user project list
func (ctrl *Controller) UserProjectList(ctx context.Context, pageIndex string, pageSize string, uid string) (*common.Pagination[Project], error) {
	wheres := []common.FilterCondition{
		{Column: "author_id", Operation: "=", Value: uid},
	}
	pagination, err := common.QueryByPage[Project](ctrl.db, pageIndex, pageSize, wheres, nil)
	if err != nil {
		return nil, err
	}
	for i := range pagination.Data {
		pagination.Data[i].Address = os.Getenv("QINIU_PATH") + "/" + pagination.Data[i].Address
	}
	return pagination, nil
}

// UpdatePublic update is_public
func (ctrl *Controller) UpdatePublic(ctx context.Context, id string, isPublic string, userId string) error {
	asset, err := common.QueryById[Asset](ctrl.db, id)
	if err != nil {
		return err
	}
	if asset.AuthorId != userId {
		return common.ErrPermissions
	}
	return UpdateProjectIsPublic(ctrl.db, id, isPublic)
}

func (ctrl *Controller) SaveAsset(ctx context.Context, asset *Asset, file multipart.File, header *multipart.FileHeader) (*Asset, error) {
	address := GetAssetAddress(asset.ID, ctrl.db)
	var data struct {
		Assets    map[string]string `json:"assets"`
		IndexJson string            `json:"indexJson"`
	}
	if err := json.Unmarshal([]byte(address), &data); err != nil {
		return nil, err
	}
	for _, value := range data.Assets {
		address = value // find /sounds/sound.wav
		break           // There will only be one sound file, so find it and return
	}
	err := ctrl.bucket.Delete(ctx, address)
	if err != nil {
		return nil, err
	}
	path, err := UploadFile(ctx, ctrl, os.Getenv("SOUNDS_PATH"), file, header.Filename)
	jsonBytes, err := json.Marshal(map[string]map[string]string{"assets": {"sound": path}})
	if err != nil {
		return nil, err
	}
	asset.Address = string(jsonBytes)
	println(asset.Address)
	if err != nil {
		return nil, err
	}
	return asset, UpdateAsset(ctrl.db, asset)
}

// SearchAsset Search Asset by name
func (ctrl *Controller) SearchAsset(ctx context.Context, search string, assetType string, userId string) ([]*Asset, error) {
	var query string
	var args []interface{}
	searchString := "%" + search + "%"

	if userId == "" {
		query = "SELECT * FROM asset WHERE name LIKE ? AND asset_type = ? AND is_public = 1"
		args = []interface{}{searchString, assetType}
	} else {
		query = "SELECT * FROM asset WHERE name LIKE ? AND asset_type = ? AND (is_public = 1 OR author_id = ?)"
		args = []interface{}{searchString, assetType, userId}
	}

	// 执行查询
	rows, err := ctrl.db.Query(query, args...)
	if err != nil {
		println(err.Error())
		return nil, err
	}
	defer rows.Close()

	// 创建指向 Asset 结构体切片的指针
	var assets []*Asset

	// 遍历结果集
	for rows.Next() {
		var asset Asset
		err := rows.Scan(&asset.ID, &asset.Name, &asset.AuthorId, &asset.Category, &asset.IsPublic, &asset.Address, &asset.AssetType, &asset.ClickCount, &asset.Status, &asset.CTime, &asset.UTime)
		if err != nil {
			println(err.Error())
			return nil, err
		}
		asset.Address, _ = ctrl.ModifyAssetAddress(asset.Address)
		// 将每行数据追加到切片中
		assets = append(assets, &asset)
	}
	if len(assets) == 0 {
		return nil, nil
	}
	return assets, nil
}

func (ctrl *Controller) ImagesToGif(ctx context.Context, files []*multipart.FileHeader) (string, error) {
	var images []*image.Paletted
	var delays []int
	for _, fileHeader := range files {
		// 打开文件
		img, err := common.LoadImage(fileHeader)
		if err != nil {
			fmt.Printf("failed to load image %s: %v", fileHeader.Filename, err)
			return "", err
		}
		images = append(images, img)
		delays = append(delays, 10) // 每帧之间的延迟，100 = 1秒
	}
	outGif := &gif.GIF{
		Image:     images,
		Delay:     delays,
		LoopCount: 0, // 循环次数，0表示无限循环
	}

	// 保存GIF文件
	f, err := os.Create("output.gif")
	if err != nil {
		fmt.Printf("failed to create GIF file: %v", err)
		return "", err
	}
	defer f.Close()
	if err := gif.EncodeAll(f, outGif); err != nil {
		fmt.Printf("failed to encode GIF: %v", err)
		return "", err
	}
	f.Seek(0, 0)
	path, err := UploadFile(ctx, ctrl, os.Getenv("GIF_PATH"), f, "output.gif")
	if err != nil {
		return "", err
	}
	return os.Getenv("QINIU_PATH") + "/" + path, err
}

// UploadSprite Upload sprite
func (ctrl *Controller) UploadSprite(ctx context.Context, name string, files []*multipart.FileHeader, gifPath string, uid string, tag string, publishState string) error {
	data := &AssetAddressData{}
	data.IndexJson = "index.json"
	if len(files) == 1 {
		data.Type = "image"
		data.Assets = make(map[string]string)
		file, _ := files[0].Open()
		path, err := UploadFile(ctx, ctrl, os.Getenv("SPRITE_PATH"), file, files[0].Filename)
		if err != nil {
			fmt.Printf("failed to upload %s: %v", files[0].Filename, err)
			return err
		}
		index := "image"
		data.Assets[index] = path
	} else {
		data.Type = "gif"
		data.Assets = make(map[string]string)
		for i, fileHeader := range files {
			file, _ := fileHeader.Open()
			path, err := UploadFile(ctx, ctrl, os.Getenv("SPRITE_PATH"), file, fileHeader.Filename)
			if err != nil {
				fmt.Printf("failed to upload %s: %v", fileHeader.Filename, err)
				return err
			}
			index := "image" + strconv.Itoa(i)
			data.Assets[index] = path
		}
		data.Url = gifPath[len(os.Getenv("QINIU_PATH")):]
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		fmt.Printf("failed to jsonMarshal: %v", err)
		return err
	}
	isPublic, _ := strconv.Atoi(publishState)

	_, err = AddAsset(ctrl.db, &Asset{
		Name:       name,
		AuthorId:   uid,
		Category:   tag,
		IsPublic:   isPublic,
		Address:    string(jsonData),
		AssetType:  "0",
		ClickCount: "0",
		Status:     1,
		CTime:      time.Now(),
		UTime:      time.Now(),
	})

	return err
}