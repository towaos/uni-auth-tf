package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/golang-jwt/jwt/v4"
)

// 環境変数から設定取得用の変数
var (
	cognitoRegion     = os.Getenv("COGNITO_REGION")
	cognitoUserPoolID = os.Getenv("COGNITO_USER_POOL_ID")
	cognitoClientID   = os.Getenv("COGNITO_CLIENT_ID")
)

// JWKSレスポンス構造体
type JWKSResponse struct {
	Keys []JWK `json:"keys"`
}

// JSON Web Key (JWK)構造体
type JWK struct {
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Kty string `json:"kty"`
	E   string `json:"e"`
	N   string `json:"n"`
	Use string `json:"use"`
}

// JWT検証に必要な情報
type TokenValidator struct {
	JwksURL     string
	UserPoolID  string
	ClientID    string
	jwksCache   map[string]*rsa.PublicKey
	lastUpdated time.Time
}

// 新しいトークン検証機能を作成
func NewTokenValidator(region, userPoolID, clientID string) (*TokenValidator, error) {
	// パラメータの検証
	if region == "" {
		return nil, errors.New("リージョンが指定されていません")
	}
	if userPoolID == "" {
		return nil, errors.New("ユーザープールIDが指定されていません")
	}
	if clientID == "" {
		return nil, errors.New("クライアントIDが指定されていません")
	}

	jwksURL := fmt.Sprintf("https://cognito-idp.%s.amazonaws.com/%s/.well-known/jwks.json", region, userPoolID)
	return &TokenValidator{
		JwksURL:    jwksURL,
		UserPoolID: userPoolID,
		ClientID:   clientID,
		jwksCache:  make(map[string]*rsa.PublicKey),
	}, nil
}

// JWKSから公開鍵を取得
func (v *TokenValidator) getPublicKey(kid string) (*rsa.PublicKey, error) {
	// キャッシュが1時間以上経過していたら更新
	if time.Since(v.lastUpdated) > time.Hour {
		v.jwksCache = make(map[string]*rsa.PublicKey)
	}

	// キャッシュに存在すればそれを返す
	if key, exists := v.jwksCache[kid]; exists {
		return key, nil
	}

	// JWKSを取得
	resp, err := http.Get(v.JwksURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// JWKSをパース
	var jwks JWKSResponse
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, err
	}

	// 対象のKIDを探す
	for _, jwk := range jwks.Keys {
		if jwk.Kid == kid {
			// RSA公開鍵を構築
			n, err := base64URLDecode(jwk.N)
			if err != nil {
				return nil, err
			}

			e, err := base64URLDecode(jwk.E)
			if err != nil {
				return nil, err
			}

			// 指数をintに変換
			var exponent int
			if len(e) < 4 {
				exponent = int(binary.BigEndian.Uint32(append(make([]byte, 4-len(e)), e...)))
			} else {
				exponent = int(binary.BigEndian.Uint32(e))
			}

			// 公開鍵を作成
			publicKey := &rsa.PublicKey{
				N: new(big.Int).SetBytes(n),
				E: exponent,
			}

			// キャッシュに保存
			v.jwksCache[jwk.Kid] = publicKey
			v.lastUpdated = time.Now()
			return publicKey, nil
		}
	}

	return nil, errors.New("適切な公開鍵が見つかりませんでした")
}

// Base64 URL デコード
func base64URLDecode(str string) ([]byte, error) {
	// パディングを調整
	if l := len(str) % 4; l > 0 {
		str += strings.Repeat("=", 4-l)
	}
	return base64.URLEncoding.DecodeString(str)
}

// JWTトークンを検証
func (v *TokenValidator) ValidateToken(tokenString string) (*jwt.Token, error) {
	// JWTのパースと検証
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// アルゴリズム確認
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("想定外の署名アルゴリズムです: %v", token.Header["alg"])
		}

		// KIDを取得
		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, errors.New("KIDが見つかりません")
		}

		// 公開鍵を取得
		return v.getPublicKey(kid)
	})

	if err != nil {
		return nil, err
	}

	// クレームを確認
	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		// 有効期限のチェック
		if !claims.VerifyExpiresAt(time.Now().Unix(), true) {
			return nil, errors.New("トークンの有効期限が切れています")
		}

		// 発行者のチェック
		issuer := fmt.Sprintf("https://cognito-idp.%s.amazonaws.com/%s", cognitoRegion, v.UserPoolID)
		if !claims.VerifyIssuer(issuer, true) {
			return nil, errors.New("トークンの発行者が一致しません")
		}

		// アプリクライアントIDのチェック（アクセストークンの場合）
		if clientID, ok := claims["client_id"]; ok && clientID != v.ClientID {
			return nil, errors.New("クライアントIDが一致しません")
		}

		// IDトークンの場合は "aud" を確認
		if aud, ok := claims["aud"]; ok && aud != v.ClientID {
			return nil, errors.New("対象者が一致しません")
		}

		return token, nil
	}

	return nil, errors.New("無効なトークンです")
}

// API Gateway Lambda オーソライザー用の関数
func ValidateTokenForAPIGateway(ctx context.Context, token string, userPoolID, clientID string) (map[string]interface{}, error) {
	if cognitoRegion == "" {
		return nil, errors.New("環境変数 COGNITO_REGION が設定されていません")
	}

	validator, err := NewTokenValidator(cognitoRegion, userPoolID, clientID)
	if err != nil {
		return nil, err
	}

	jwtToken, err := validator.ValidateToken(token)
	if err != nil {
		return nil, err
	}

	// 検証成功時にはクレームを返却
	claims, _ := jwtToken.Claims.(jwt.MapClaims)
	return claims, nil
}

// Lambda用リクエストハンドラ
func handleRequest(ctx context.Context, event events.APIGatewayCustomAuthorizerRequest) (events.APIGatewayCustomAuthorizerResponse, error) {
	// トークンを取得（「Bearer 」部分を削除）
	token := strings.TrimPrefix(event.AuthorizationToken, "Bearer ")

	// トークン検証
	claims, err := ValidateTokenForAPIGateway(ctx, token, cognitoUserPoolID, cognitoClientID)
	if err != nil {
		return events.APIGatewayCustomAuthorizerResponse{}, err
	}

	// ユーザー識別子を取得
	sub, _ := claims["sub"].(string)

	// ポリシードキュメントを生成
	policyDocument := generatePolicy(sub, "Allow", event.MethodArn)

	// コンテキストに追加情報を設定
	context := map[string]interface{}{
		"sub": sub,
	}

	// カスタム属性があれば追加
	if username, ok := claims["cognito:username"].(string); ok {
		context["username"] = username
	}

	if email, ok := claims["email"].(string); ok {
		context["email"] = email
	}

	// レスポンスを作成
	authResponse := events.APIGatewayCustomAuthorizerResponse{
		PrincipalID:    sub,
		PolicyDocument: policyDocument,
		Context:        context,
	}

	return authResponse, nil
}

// IAMポリシードキュメントを生成
func generatePolicy(principalID, effect, resource string) events.APIGatewayCustomAuthorizerPolicy {
	// ベースとなるポリシーを作成
	policyDocument := events.APIGatewayCustomAuthorizerPolicy{
		Version: "2012-10-17",
		Statement: []events.IAMPolicyStatement{
			{
				Action:   []string{"execute-api:Invoke"},
				Effect:   effect,
				Resource: []string{resource},
			},
		},
	}

	return policyDocument
}

// main関数
func main() {
	lambda.Start(handleRequest)
}
